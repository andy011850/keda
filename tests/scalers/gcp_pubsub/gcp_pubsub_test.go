//go:build e2e
// +build e2e

package gcp_pubsub_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/joho/godotenv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/kubernetes"

	. "github.com/kedacore/keda/v2/tests/helper"
)

// Load environment variables from .env file
var _ = godotenv.Load("../../.env")

var now = time.Now().UnixNano()

const (
	testName = "gcp-pubsub-test"
)

var (
	gcpKey              = os.Getenv("GCP_SP_KEY")
	creds               = make(map[string]interface{})
	errGcpKey           = json.Unmarshal([]byte(gcpKey), &creds)
	testNamespace       = fmt.Sprintf("%s-ns", testName)
	secretName          = fmt.Sprintf("%s-secret", testName)
	deploymentName      = fmt.Sprintf("%s-deployment", testName)
	scaledObjectName    = fmt.Sprintf("%s-so", testName)
	projectID           = creds["project_id"]
	topicID             = fmt.Sprintf("projects/%s/topics/keda-test-topic-%d", projectID, now)
	subscriptionName    = fmt.Sprintf("keda-test-topic-sub-%d", now)
	subscriptionID      = fmt.Sprintf("projects/%s/subscriptions/%s", projectID, subscriptionName)
	maxReplicaCount     = 4
	activationThreshold = 5
	gsPrefix            = fmt.Sprintf("kubectl exec --namespace %s deploy/gcp-sdk -- ", testNamespace)
)

type templateData struct {
	TestNamespace       string
	SecretName          string
	GcpCreds            string
	DeploymentName      string
	ScaledObjectName    string
	SubscriptionName    string
	SubscriptionID      string
	MaxReplicaCount     int
	ActivationThreshold int
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
  creds.json: {{.GcpCreds}}
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
        - name: {{.DeploymentName}}-processor
          image: google/cloud-sdk:slim
          # Consume a message
          command: [ "/bin/bash", "-c", "--" ]
          args: [ "gcloud auth activate-service-account --key-file /etc/secret-volume/creds.json && \
          while true; do gcloud pubsub subscriptions pull {{.SubscriptionID}} --auto-ack; sleep 20; done" ]
          env:
            - name: GOOGLE_APPLICATION_CREDENTIALS_JSON
              valueFrom:
                secretKeyRef:
                  name: {{.SecretName}}
                  key: creds.json
          volumeMounts:
            - name: secret-volume
              mountPath: /etc/secret-volume
      volumes:
        - name: secret-volume
          secret:
            secretName: {{.SecretName}}
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
  minReplicaCount: 0
  maxReplicaCount: {{.MaxReplicaCount}}
  cooldownPeriod: 10
  triggers:
    - type: gcp-pubsub
      metadata:
        subscriptionName: {{.SubscriptionName}}
        mode: SubscriptionSize
        value: "5"
        activationValue: "{{.ActivationThreshold}}"
        credentialsFromEnv: GOOGLE_APPLICATION_CREDENTIALS_JSON
`

	gcpSdkTemplate = `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: gcp-sdk
  namespace: {{.TestNamespace}}
  labels:
    app: gcp-sdk
spec:
  replicas: 1
  selector:
    matchLabels:
      app: gcp-sdk
  template:
    metadata:
      labels:
        app: gcp-sdk
    spec:
      containers:
        - name: gcp-sdk-container
          image: google/cloud-sdk:slim
          # Just spin & wait forever
          command: [ "/bin/bash", "-c", "--" ]
          args: [ "ls /tmp && while true; do sleep 30; done;" ]
          volumeMounts:
            - name: secret-volume
              mountPath: /etc/secret-volume
      volumes:
        - name: secret-volume
          secret:
            secretName: {{.SecretName}}
`
)

func TestScaler(t *testing.T) {
	// setup
	t.Log("--- setting up ---")
	require.NotEmpty(t, gcpKey, "GCP_KEY env variable is required for GCP storage test")
	assert.NoErrorf(t, errGcpKey, "Failed to load credentials from gcpKey - %s", errGcpKey)

	// Create kubernetes resources
	kc := GetKubernetesClient(t)
	data, templates := getTemplateData()

	CreateKubernetesResources(t, kc, testNamespace, data, templates)

	assert.True(t, WaitForDeploymentReplicaReadyCount(t, kc, deploymentName, testNamespace, 0, 60, 1),
		"replica count should be 0 after a minute")

	sdkReady := WaitForDeploymentReplicaReadyCount(t, kc, "gcp-sdk", testNamespace, 1, 60, 1)
	assert.True(t, sdkReady, "gcp-sdk deployment should be ready after a minute")

	if sdkReady {
		if createPubsub(t) == nil {
			// test scaling
			testActivation(t, kc)
			testScaleUp(t, kc)
			testScaleDown(t, kc)

			// cleanup
			t.Log("--- cleanup ---")
			cleanupPubsub(t)
		}
	}

	DeleteKubernetesResources(t, kc, testNamespace, data, templates)
}

func createPubsub(t *testing.T) error {
	// Authenticate to GCP
	t.Log("--- authenticate to GCP ---")
	cmd := fmt.Sprintf("%sgcloud auth activate-service-account %s --key-file /etc/secret-volume/creds.json --project=%s", gsPrefix, creds["client_email"], projectID)
	_, err := ExecuteCommand(cmd)
	assert.NoErrorf(t, err, "Failed to set GCP authentication on gcp-sdk - %s", err)
	if err != nil {
		return err
	}

	// Create topic
	t.Log("--- create topic ---")
	cmd = fmt.Sprintf("%sgcloud pubsub topics create %s", gsPrefix, topicID)
	_, err = ExecuteCommand(cmd)
	assert.NoErrorf(t, err, "Failed to create Pubsub topic %s: %s", topicID, err)
	if err != nil {
		return err
	}

	// Create subscription
	t.Log("--- create subscription ---")
	cmd = fmt.Sprintf("%sgcloud pubsub subscriptions create %s --topic=%s", gsPrefix, subscriptionID, topicID)
	_, err = ExecuteCommand(cmd)
	assert.NoErrorf(t, err, "Failed to create Pubsub subscription %s: %s", subscriptionID, err)

	return err
}

func cleanupPubsub(t *testing.T) {
	// Delete the topic and subscription
	t.Log("--- cleaning up the subscription and topic ---")
	_, _ = ExecuteCommand(fmt.Sprintf("%sgcloud pubsub subscriptions delete %s", gsPrefix, subscriptionID))
	_, _ = ExecuteCommand(fmt.Sprintf("%sgcloud pubsub topics delete %s", gsPrefix, topicID))
}

func getTemplateData() (templateData, templateValues) {
	base64GcpCreds := base64.StdEncoding.EncodeToString([]byte(gcpKey))

	return templateData{
			TestNamespace:       testNamespace,
			SecretName:          secretName,
			GcpCreds:            base64GcpCreds,
			DeploymentName:      deploymentName,
			ScaledObjectName:    scaledObjectName,
			SubscriptionID:      subscriptionID,
			SubscriptionName:    subscriptionName,
			MaxReplicaCount:     maxReplicaCount,
			ActivationThreshold: activationThreshold,
		}, templateValues{
			"secretTemplate":       secretTemplate,
			"deploymentTemplate":   deploymentTemplate,
			"scaledObjectTemplate": scaledObjectTemplate,
			"gcpSdkTemplate":       gcpSdkTemplate}
}

func publishMessages(t *testing.T, count int) {
	t.Logf("--- publishing %d messages ---", count)
	publish := fmt.Sprintf(
		"%s/bin/bash -c -- 'for i in {1..%d}; do gcloud pubsub topics publish %s --message=AAAAAAAAAA;done'",
		gsPrefix,
		count,
		topicID)
	_, err := ExecuteCommand(publish)
	assert.NoErrorf(t, err, "cannot publish messages to pubsub topic - %s", err)
}

func testActivation(t *testing.T, kc *kubernetes.Clientset) {
	t.Log("--- testing not scaling if below threshold ---")

	publishMessages(t, activationThreshold)

	t.Log("--- waiting to see replicas are not scaled up ---")
	AssertReplicaCountNotChangeDuringTimePeriod(t, kc, deploymentName, testNamespace, 0, 240)
}

func testScaleUp(t *testing.T, kc *kubernetes.Clientset) {
	t.Log("--- testing scale up ---")

	publishMessages(t, 20-activationThreshold)

	t.Log("--- waiting for replicas to scale up ---")
	assert.True(t, WaitForDeploymentReplicaReadyCount(t, kc, deploymentName, testNamespace, maxReplicaCount, 30, 10),
		fmt.Sprintf("replica count should be %d after five minutes", maxReplicaCount))
}

func testScaleDown(t *testing.T, kc *kubernetes.Clientset) {
	t.Log("--- testing scale down ---")
	cmd := fmt.Sprintf("%sgcloud pubsub subscriptions seek %s --time=-P1S", gsPrefix, subscriptionID)
	_, err := ExecuteCommand(cmd)
	assert.NoErrorf(t, err, "cannot reset subscription position - %s", err)

	t.Log("--- waiting for replicas to scale down to zero ---")
	assert.True(t, WaitForDeploymentReplicaReadyCount(t, kc, deploymentName, testNamespace, 0, 30, 10),
		"replica count should be 0 after five minute")
}
