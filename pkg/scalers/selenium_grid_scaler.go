package scalers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-logr/logr"
	v2beta2 "k8s.io/api/autoscaling/v2beta2"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/metrics/pkg/apis/external_metrics"

	kedautil "github.com/kedacore/keda/v2/pkg/util"
)

type seleniumGridScaler struct {
	metricType v2beta2.MetricTargetType
	metadata   *seleniumGridScalerMetadata
	client     *http.Client
	logger     logr.Logger
}

type seleniumGridScalerMetadata struct {
	url                 string
	browserName         string
	sessionBrowserName  string
	targetValue         int64
	activationThreshold int64
	platformName        string
	unsafeSsl           bool
	scalerIndex         int
	avd                 string
}

type seleniumResponse struct {
	Data data `json:"data"`
}

type data struct {
	Grid         grid         `json:"grid"`
	SessionsInfo sessionsInfo `json:"sessionsInfo"`
}

type grid struct {
	MaxSession int `json:"maxSession"`
	NodeCount  int `json:"nodeCount"`
}

type sessionsInfo struct {
	SessionQueueRequests []string          `json:"sessionQueueRequests"`
	Sessions             []seleniumSession `json:"sessions"`
}

type seleniumSession struct {
	ID           string `json:"id"`
	Capabilities string `json:"capabilities"`
	NodeID       string `json:"nodeId"`
}

type capability struct {
	BrowserName  string `json:"browserName"`
	PlatformName string `json:"platformName"`
	Avd          string `json:"appium:avd"`
}

const (
	DefaultPlatformName string = "LINUX"
)

func NewSeleniumGridScaler(config *ScalerConfig) (Scaler, error) {
	metricType, err := GetMetricTargetType(config)
	if err != nil {
		return nil, fmt.Errorf("error getting scaler metric type: %s", err)
	}

	logger := InitializeLogger(config, "selenium_grid_scaler")

	meta, err := parseSeleniumGridScalerMetadata(config)

	if err != nil {
		return nil, fmt.Errorf("error parsing selenium grid metadata: %s", err)
	}

	httpClient := kedautil.CreateHTTPClient(config.GlobalHTTPTimeout, meta.unsafeSsl)

	return &seleniumGridScaler{
		metricType: metricType,
		metadata:   meta,
		client:     httpClient,
		logger:     logger,
	}, nil
}

func parseSeleniumGridScalerMetadata(config *ScalerConfig) (*seleniumGridScalerMetadata, error) {
	meta := seleniumGridScalerMetadata{
		targetValue: 1,
	}

	if val, ok := config.TriggerMetadata["url"]; ok {
		meta.url = val
	} else {
		return nil, fmt.Errorf("no selenium grid url given in metadata")
	}

	if val, ok := config.TriggerMetadata["browserName"]; ok {
		meta.browserName = val
	} else {
		return nil, fmt.Errorf("no browser name given in metadata")
	}

	if val, ok := config.TriggerMetadata["sessionBrowserName"]; ok {
		meta.sessionBrowserName = val
	} else {
		meta.sessionBrowserName = meta.browserName
	}

	meta.activationThreshold = 0
	if val, ok := config.TriggerMetadata["activationThreshold"]; ok {
		activationThreshold, err := strconv.ParseInt(val, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("error parsing unsafeSsl: %s", err)
		}
		meta.activationThreshold = activationThreshold
	}

	if val, ok := config.TriggerMetadata["platformName"]; ok && val != "" {
		meta.platformName = val
	} else {
		meta.platformName = DefaultPlatformName
	}

	if val, ok := config.TriggerMetadata["appium:avd"]; ok && val != "" {
		meta.avd = val
	}

	if val, ok := config.TriggerMetadata["unsafeSsl"]; ok {
		parsedVal, err := strconv.ParseBool(val)
		if err != nil {
			return nil, fmt.Errorf("error parsing unsafeSsl: %s", err)
		}
		meta.unsafeSsl = parsedVal
	}

	meta.scalerIndex = config.ScalerIndex
	return &meta, nil
}

// No cleanup required for selenium grid scaler
func (s *seleniumGridScaler) Close(context.Context) error {
	return nil
}

func (s *seleniumGridScaler) GetMetrics(ctx context.Context, metricName string, metricSelector labels.Selector) ([]external_metrics.ExternalMetricValue, error) {
	sessions, err := s.getSessionsCount(ctx, s.logger)
	if err != nil {
		return []external_metrics.ExternalMetricValue{}, fmt.Errorf("error requesting selenium grid endpoint: %s", err)
	}

	metric := GenerateMetricInMili(metricName, float64(sessions))

	return append([]external_metrics.ExternalMetricValue{}, metric), nil
}

func (s *seleniumGridScaler) GetMetricSpecForScaling(context.Context) []v2beta2.MetricSpec {
	metricName := kedautil.NormalizeString(fmt.Sprintf("seleniumgrid-%s", s.metadata.browserName))
	externalMetric := &v2beta2.ExternalMetricSource{
		Metric: v2beta2.MetricIdentifier{
			Name: GenerateMetricNameWithIndex(s.metadata.scalerIndex, metricName),
		},
		Target: GetMetricTarget(s.metricType, s.metadata.targetValue),
	}
	metricSpec := v2beta2.MetricSpec{
		External: externalMetric, Type: externalMetricType,
	}
	return []v2beta2.MetricSpec{metricSpec}
}

func (s *seleniumGridScaler) IsActive(ctx context.Context) (bool, error) {
	v, err := s.getSessionsCount(ctx, s.logger)
	if err != nil {
		return false, err
	}

	return v > s.metadata.activationThreshold, nil
}

func (s *seleniumGridScaler) getSessionsCount(ctx context.Context, logger logr.Logger) (int64, error) {
	body, err := json.Marshal(map[string]string{
		"query": "{ grid { maxSession, nodeCount }, sessionsInfo { sessionQueueRequests, sessions { id, capabilities, nodeId } } }",
	})

	if err != nil {
		return -1, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", s.metadata.url, bytes.NewBuffer(body))
	if err != nil {
		return -1, err
	}

	res, err := s.client.Do(req)
	if err != nil {
		return -1, err
	}

	if res.StatusCode != http.StatusOK {
		msg := fmt.Sprintf("selenium grid returned %d", res.StatusCode)
		return -1, errors.New(msg)
	}

	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		return -1, err
	}
	v, err := getCountFromSeleniumResponse(b, s.metadata.browserName, s.metadata.platformName, s.metadata.avd, s.metadata.sessionBrowserName, logger)
	if err != nil {
		return -1, err
	}
	return v, nil
}

func getCountFromSeleniumResponse(b []byte, browserName string, platformName string, avd string, sessionBrowserName string, logger logr.Logger) (int64, error) {
	var count int64
	var seleniumResponse = seleniumResponse{}

	if err := json.Unmarshal(b, &seleniumResponse); err != nil {
		return 0, err
	}

	var sessionQueueRequests = seleniumResponse.Data.SessionsInfo.SessionQueueRequests
	for _, sessionQueueRequest := range sessionQueueRequests {
		var capability = capability{}
		if err := json.Unmarshal([]byte(sessionQueueRequest), &capability); err == nil {
			if capability.BrowserName == browserName {
				if strings.HasPrefix(capability.PlatformName, platformName) {
					if strings.HasPrefix(capability.Avd, avd) {
						count++
					}
					// count++
				} else if platformName == DefaultPlatformName {
					if strings.HasPrefix(capability.Avd, avd) {
						count++
					}
					// count++
				}
			}
		} else {
			logger.Error(err, fmt.Sprintf("Error when unmarshaling session queue requests: %s", err))
		}
	}

	var sessions = seleniumResponse.Data.SessionsInfo.Sessions
	for _, session := range sessions {
		var capability = capability{}
		if err := json.Unmarshal([]byte(session.Capabilities), &capability); err == nil {
			if capability.BrowserName == browserName {
				if strings.HasPrefix(capability.PlatformName, platformName) {
					if strings.HasPrefix(capability.Avd, avd) {
						count++
					}
					// count++
				} else if platformName == DefaultPlatformName {
					if strings.HasPrefix(capability.Avd, avd) {
						count++
					}
					// count++
				}
			}
		} else {
			logger.Error(err, fmt.Sprintf("Error when unmarshaling sessions info: %s", err))
		}
	}

	var gridMaxSession = int64(seleniumResponse.Data.Grid.MaxSession)
	var gridNodeCount = int64(seleniumResponse.Data.Grid.NodeCount)

	if gridMaxSession > 0 && gridNodeCount > 0 {
		// Get count, convert count to next highest int64
		var floatCount = float64(count) / (float64(gridMaxSession) / float64(gridNodeCount))
		count = int64(math.Ceil(floatCount))
	}
	return count, nil
}