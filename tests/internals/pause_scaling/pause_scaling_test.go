//go:build e2e
// +build e2e

package pause_scaling_test

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Azure/azure-storage-queue-go/azqueue"
	"github.com/joho/godotenv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/kubernetes"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	"github.com/kedacore/keda/v2/pkg/scalers/azure"
	kedautil "github.com/kedacore/keda/v2/pkg/util"
	. "github.com/kedacore/keda/v2/tests/helper"
)

// Load environment variables from .env file
var _ = godotenv.Load("../../.env")

const (
	testName = "pause-scaling-test"
)

var (
	connectionString = os.Getenv("AZURE_STORAGE_CONNECTION_STRING")
	testNamespace    = fmt.Sprintf("%s-ns", testName)
	secretName       = fmt.Sprintf("%s-secret", testName)
	deploymentName   = fmt.Sprintf("%s-deployment", testName)
	scaledObjectName = fmt.Sprintf("%s-so", testName)
	queueName        = fmt.Sprintf("%s-queue", testName)
)

type templateData struct {
	TestNamespace      string
	SecretName         string
	Connection         string
	DeploymentName     string
	ScaledObjectName   string
	QueueName          string
	PausedReplicaCount int
}
type templateValues map[string]string

const (
	secretTemplate = `
apiVersion: v1
kind: Secret
metadata:
  name: {{.SecretName}}
  namespace: {{.TestNamespace}}
data:
  AzureWebJobsStorage: {{.Connection}}
`

	deploymentTemplate = `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{.DeploymentName}}
  namespace: {{.TestNamespace}}
  labels:
    app: {{.DeploymentName}}
spec:
  replicas: 0
  selector:
    matchLabels:
      app: {{.DeploymentName}}
  template:
    metadata:
      labels:
        app: {{.DeploymentName}}
    spec:
      containers:
        - name: {{.DeploymentName}}
          image: ghcr.io/kedacore/tests-azure-queue
          resources:
          env:
            - name: FUNCTIONS_WORKER_RUNTIME
              value: node
            - name: AzureWebJobsStorage
              valueFrom:
                secretKeyRef:
                  name: {{.SecretName}}
                  key: AzureWebJobsStorage
`

	scaledObjectTemplate = `
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: {{.ScaledObjectName}}
  namespace: {{.TestNamespace}}
spec:
  scaleTargetRef:
    name: {{.DeploymentName}}
  pollingInterval: 5
  minReplicaCount: 2
  maxReplicaCount: 4
  cooldownPeriod: 10
  triggers:
    - type: azure-queue
      metadata:
        queueName: {{.QueueName}}
        connectionFromEnv: AzureWebJobsStorage
`

	scaledObjectAnnotatedTemplate = `
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: {{.ScaledObjectName}}
  namespace: {{.TestNamespace}}
  annotations:
    autoscaling.keda.sh/paused-replicas: "{{.PausedReplicaCount}}"
spec:
  scaleTargetRef:
    name: {{.DeploymentName}}
  pollingInterval: 5
  minReplicaCount: 2
  maxReplicaCount: 4
  cooldownPeriod: 10
  triggers:
    - type: azure-queue
      metadata:
        queueName: {{.QueueName}}
        connectionFromEnv: AzureWebJobsStorage
`
)

func TestScaler(t *testing.T) {
	// setup
	t.Log("--- setting up ---")
	require.NotEmpty(t, connectionString, "AZURE_STORAGE_CONNECTION_STRING env variable is required for pause scaling test")

	queueURL, messageURL := createQueue(t)

	// Create kubernetes resources
	kc := GetKubernetesClient(t)
	data, templates := getTemplateData()

	CreateKubernetesResources(t, kc, testNamespace, data, templates)

	// scaling to paused replica count
	assert.True(t, WaitForDeploymentReplicaCount(t, kc, deploymentName, testNamespace, 0, 60, 1),
		"replica count should be 0 after a minute")

	// test scaling
	testPauseAt0(t, kc, messageURL)
	testScaleUp(t, kc, data)
	testPauseAtN(t, kc, messageURL, data, 5)
	testScaleDown(t, kc, data)

	// cleanup
	DeleteKubernetesResources(t, kc, testNamespace, data, templates)
	cleanupQueue(t, queueURL)
}

func createQueue(t *testing.T) (azqueue.QueueURL, azqueue.MessagesURL) {
	// Create Queue
	httpClient := kedautil.CreateHTTPClient(DefaultHTTPTimeOut, false)
	credential, endpoint, err := azure.ParseAzureStorageQueueConnection(
		context.Background(), httpClient, kedav1alpha1.AuthPodIdentity{Provider: kedav1alpha1.PodIdentityProviderNone},
		connectionString, "", "")
	assert.NoErrorf(t, err, "cannot parse storage connection string - %s", err)

	p := azqueue.NewPipeline(credential, azqueue.PipelineOptions{})
	serviceURL := azqueue.NewServiceURL(*endpoint, p)
	queueURL := serviceURL.NewQueueURL(queueName)

	_, err = queueURL.Create(context.Background(), azqueue.Metadata{})
	assert.NoErrorf(t, err, "cannot create storage queue - %s", err)

	messageURL := queueURL.NewMessagesURL()

	return queueURL, messageURL
}

func getTemplateData() (templateData, templateValues) {
	base64ConnectionString := base64.StdEncoding.EncodeToString([]byte(connectionString))

	return templateData{
			TestNamespace:      testNamespace,
			SecretName:         secretName,
			Connection:         base64ConnectionString,
			DeploymentName:     deploymentName,
			ScaledObjectName:   scaledObjectName,
			QueueName:          queueName,
			PausedReplicaCount: 0,
		}, templateValues{
			"secretTemplate":                secretTemplate,
			"deploymentTemplate":            deploymentTemplate,
			"scaledObjectAnnotatedTemplate": scaledObjectAnnotatedTemplate}
}

func testPauseAt0(t *testing.T, kc *kubernetes.Clientset, messageURL azqueue.MessagesURL) {
	t.Log("--- testing pausing at 0 ---")
	for i := 0; i < 5; i++ {
		msg := fmt.Sprintf("Message - %d", i)
		_, err := messageURL.Enqueue(context.Background(), msg, 0*time.Second, time.Hour)
		assert.NoErrorf(t, err, "cannot enqueue message - %s", err)
	}

	assert.True(t, WaitForDeploymentReplicaCountChange(t, kc, deploymentName, testNamespace, 60, 1) == 0,
		"replica count should stay at 0")
}

func testScaleUp(t *testing.T, kc *kubernetes.Clientset, data templateData) {
	t.Log("--- testing scale up ---")
	KubectlApplyWithTemplate(t, data, "scaledObjectTemplate", scaledObjectTemplate)

	assert.True(t, WaitForDeploymentReplicaCount(t, kc, deploymentName, testNamespace, 2, 60, 1),
		"replica count should be 2 after a minute")
}

func testPauseAtN(t *testing.T, kc *kubernetes.Clientset, messageURL azqueue.MessagesURL, data templateData, n int) {
	t.Log("--- testing pausing at N ---")
	data.PausedReplicaCount = n
	KubectlApplyWithTemplate(t, data, "scaledObjectAnnotatedTemplate", scaledObjectAnnotatedTemplate)

	_, err := messageURL.Clear(context.Background())
	assert.NoErrorf(t, err, "cannot clear queue - %s", err)

	assert.Truef(t, WaitForDeploymentReplicaCountChange(t, kc, deploymentName, testNamespace, 60, 1) == n,
		"replica count should stay at %d", n)
}

func testScaleDown(t *testing.T, kc *kubernetes.Clientset, data templateData) {
	t.Log("--- testing scale down ---")
	KubectlApplyWithTemplate(t, data, "scaledObjectTemplate", scaledObjectTemplate)

	assert.True(t, WaitForDeploymentReplicaCount(t, kc, deploymentName, testNamespace, 2, 60, 1),
		"replica count should be 2 after a minute")
}

func cleanupQueue(t *testing.T, queueURL azqueue.QueueURL) {
	t.Log("--- cleaning up ---")
	_, err := queueURL.Delete(context.Background())
	assert.NoErrorf(t, err, "cannot create storage queue - %s", err)
}
