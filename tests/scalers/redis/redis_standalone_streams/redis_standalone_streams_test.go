//go:build e2e
// +build e2e

package redis_standalone_streams_test

import (
	"encoding/base64"
	"fmt"
	"testing"

	"github.com/joho/godotenv"
	"github.com/stretchr/testify/assert"
	"k8s.io/client-go/kubernetes"

	. "github.com/kedacore/keda/v2/tests/helper"
	redis "github.com/kedacore/keda/v2/tests/scalers/redis/helper"
)

// Load environment variables from .env file
var _ = godotenv.Load("../../.env")

const (
	testName = "redis-standalone-streams-test"
)

var (
	testNamespace             = fmt.Sprintf("%s-ns", testName)
	redisNamespace            = fmt.Sprintf("%s-redis-ns", testName)
	deploymentName            = fmt.Sprintf("%s-deployment", testName)
	jobName                   = fmt.Sprintf("%s-job", testName)
	scaledObjectName          = fmt.Sprintf("%s-so", testName)
	triggerAuthenticationName = fmt.Sprintf("%s-ta", testName)
	secretName                = fmt.Sprintf("%s-secret", testName)
	redisPassword             = "admin"
	redisStreamName           = "stream"
	redisHost                 = fmt.Sprintf("redis.%s.svc.cluster.local:6379", redisNamespace)
	minReplicaCount           = 1
	maxReplicaCount           = 2
)

type templateData struct {
	TestNamespace             string
	DeploymentName            string
	JobName                   string
	ScaledObjectName          string
	TriggerAuthenticationName string
	SecretName                string
	MinReplicaCount           int
	MaxReplicaCount           int
	RedisPassword             string
	RedisPasswordBase64       string
	RedisStreamName           string
	RedisHost                 string
	ItemsToWrite              int
}

type templateValues map[string]string

const (
	deploymentTemplate = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{.DeploymentName}}
  namespace: {{.TestNamespace}}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: {{.DeploymentName}}
  template:
    metadata:
      labels:
        app: {{.DeploymentName}}
    spec:
      containers:
      - name: redis-worker
        image: abhirockzz/redis-streams-consumer
        imagePullPolicy: IfNotPresent
        env:
        - name: REDIS_HOST
          value: {{.RedisHost}}
        - name: REDIS_STREAM_NAME
          value: {{.RedisStreamName}}
        - name: REDIS_PASSWORD
          value: {{.RedisPassword}}
        - name: REDIS_STREAM_CONSUMER_GROUP_NAME
          value: "consumer-group-1"
`

	secretTemplate = `apiVersion: v1
kind: Secret
metadata:
  name: {{.SecretName}}
  namespace: {{.TestNamespace}}
type: Opaque
data:
  password: {{.RedisPasswordBase64}}
`

	triggerAuthenticationTemplate = `apiVersion: keda.sh/v1alpha1
kind: TriggerAuthentication
metadata:
  name: {{.TriggerAuthenticationName}}
  namespace: {{.TestNamespace}}
spec:
  secretTargetRef:
  - parameter: password
    name: {{.SecretName}}
    key: password
`

	scaledObjectTemplate = `apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: {{.ScaledObjectName}}
  namespace: {{.TestNamespace}}
spec:
  scaleTargetRef:
    name: {{.DeploymentName}}
  pollingInterval: 5
  cooldownPeriod:  10
  minReplicaCount: {{.MinReplicaCount}}
  maxReplicaCount: {{.MaxReplicaCount}}
  advanced:
    horizontalPodAutoscalerConfig:
      behavior:
        scaleDown:
          stabilizationWindowSeconds: 15
  triggers:
  - type: redis-streams
    metadata:
      addressFromEnv: REDIS_HOST
      stream: {{.RedisStreamName}}
      consumerGroup: consumer-group-1
      pendingEntriesCount: "10"
    authenticationRef:
      name: {{.TriggerAuthenticationName}}
`

	insertJobTemplate = `apiVersion: batch/v1
kind: Job
metadata:
  name: {{.JobName}}
  namespace: {{.TestNamespace}}
spec:
  ttlSecondsAfterFinished: 0
  template:
    spec:
      containers:
      - name: redis
        image: abhirockzz/redis-streams-producer
        imagePullPolicy: IfNotPresent
        env:
        - name: REDIS_HOST
          value: {{.RedisHost}}
        - name: REDIS_PASSWORD
          value: {{.RedisPassword}}
        - name: REDIS_STREAM_NAME
          value: {{.RedisStreamName}}
        - name: NUM_MESSAGES
          value: "{{.ItemsToWrite}}"
      restartPolicy: Never
  backoffLimit: 4
`
)

func TestScaler(t *testing.T) {
	// Create kubernetes resources for PostgreSQL server
	kc := GetKubernetesClient(t)

	// Create Redis Standalone
	redis.InstallStandalone(t, kc, testName, redisNamespace, redisPassword)

	// Create kubernetes resources for testing
	data, templates := getTemplateData()
	CreateKubernetesResources(t, kc, testNamespace, data, templates)
	assert.True(t, WaitForDeploymentReplicaReadyCount(t, kc, deploymentName, testNamespace, 1, 60, 3),
		"replica count should be %d after 3 minutes", 1)

	testScaleUp(t, kc, data)
	testScaleDown(t, kc)

	// cleanup
	redis.RemoveStandalone(t, kc, testName, redisNamespace)
	DeleteKubernetesResources(t, kc, testNamespace, data, templates)
}

func testScaleUp(t *testing.T, kc *kubernetes.Clientset, data templateData) {
	t.Log("--- testing scale up ---")
	templateTriggerJob := templateValues{"insertJobTemplate": insertJobTemplate}
	data.ItemsToWrite = 20
	KubectlApplyMultipleWithTemplate(t, data, templateTriggerJob)

	assert.True(t, WaitForDeploymentReplicaReadyCount(t, kc, deploymentName, testNamespace, maxReplicaCount, 60, 3),
		"replica count should be %d after 3 minutes", maxReplicaCount)
}

func testScaleDown(t *testing.T, kc *kubernetes.Clientset) {
	t.Log("--- testing scale down ---")

	assert.True(t, WaitForDeploymentReplicaReadyCount(t, kc, deploymentName, testNamespace, minReplicaCount, 60, 3),
		"replica count should be %d after 5 minutes", minReplicaCount)
}

var data = templateData{
	TestNamespace:             testNamespace,
	DeploymentName:            deploymentName,
	ScaledObjectName:          scaledObjectName,
	MinReplicaCount:           1,
	MaxReplicaCount:           maxReplicaCount,
	TriggerAuthenticationName: triggerAuthenticationName,
	SecretName:                secretName,
	JobName:                   jobName,
	RedisPassword:             redisPassword,
	RedisPasswordBase64:       base64.StdEncoding.EncodeToString([]byte(redisPassword)),
	RedisStreamName:           redisStreamName,
	RedisHost:                 redisHost,
	ItemsToWrite:              0,
}

func getTemplateData() (templateData, map[string]string) {
	return data, templateValues{
		"secretTemplate":                secretTemplate,
		"deploymentTemplate":            deploymentTemplate,
		"triggerAuthenticationTemplate": triggerAuthenticationTemplate,
		"scaledObjectTemplate":          scaledObjectTemplate,
	}
}
