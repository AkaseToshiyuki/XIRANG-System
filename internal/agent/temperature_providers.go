package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type CPUTemperatureReading struct {
	Celsius    float64
	Source     string
	Confidence string
}

type CPUTemperatureProvider interface {
	ReadCPUTemperature(context.Context) (CPUTemperatureReading, error)
}

type IPMITemperatureProvider struct {
	Args    []string
	Command string
}

func (provider IPMITemperatureProvider) ReadCPUTemperature(ctx context.Context) (CPUTemperatureReading, error) {
	command := strings.TrimSpace(provider.Command)
	if command == "" {
		return CPUTemperatureReading{}, errors.New("IPMI command is not configured")
	}
	probe, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	output, err := runHealthCommand(probe, command, append(append([]string(nil), provider.Args...), "sdr", "type", "Temperature")...)
	if err != nil {
		return CPUTemperatureReading{}, fmt.Errorf("IPMI temperature query: %w", err)
	}
	value := parseIPMICPUTemperature(string(output))
	if value == 0 {
		return CPUTemperatureReading{}, errors.New("IPMI returned no CPU temperature sensor")
	}
	return CPUTemperatureReading{Celsius: value, Source: "ipmi", Confidence: "medium"}, nil
}

func parseIPMICPUTemperature(output string) float64 {
	maximum := float64(0)
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Split(line, "|")
		if len(fields) < 2 {
			continue
		}
		label := strings.ToLower(strings.TrimSpace(fields[0]))
		if !cpuSensorLabel(label) {
			continue
		}
		reading := fields[1]
		// sdr type Temperature uses the extended five-column form: name,
		// sensor number, status, entity, reading. Never parse the sensor ID.
		if len(fields) >= 5 {
			reading = fields[4]
		}
		valueFields := strings.Fields(strings.TrimSpace(reading))
		if len(valueFields) < 3 || !strings.EqualFold(valueFields[1], "degrees") || valueFields[2] != "C" {
			continue
		}
		value, err := strconv.ParseFloat(valueFields[0], 64)
		if err == nil && validTemperature(value) && value > maximum {
			maximum = value
		}
	}
	return maximum
}

type RedfishTemperatureProvider struct {
	URL      string
	Username string
	Password string
	Client   *http.Client
}

type redfishThermalResponse struct {
	Temperatures []struct {
		Name            string  `json:"Name"`
		PhysicalContext string  `json:"PhysicalContext"`
		ReadingCelsius  float64 `json:"ReadingCelsius"`
		Status          struct {
			Health string `json:"Health"`
			State  string `json:"State"`
		} `json:"Status"`
	} `json:"Temperatures"`
}

func (provider RedfishTemperatureProvider) ReadCPUTemperature(ctx context.Context) (CPUTemperatureReading, error) {
	parsed, err := url.Parse(provider.URL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || provider.Username == "" || provider.Password == "" {
		return CPUTemperatureReading{}, errors.New("the Redfish provider requires an HTTPS URL and credentials")
	}
	requestContext, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return CPUTemperatureReading{}, err
	}
	request.SetBasicAuth(provider.Username, provider.Password)
	request.Header.Set("Accept", "application/json")
	client := provider.Client
	if client == nil {
		client = &http.Client{Timeout: 4 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return CPUTemperatureReading{}, fmt.Errorf("query the Redfish temperature service: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return CPUTemperatureReading{}, fmt.Errorf("the Redfish temperature query returned HTTP %d", response.StatusCode)
	}
	var payload redfishThermalResponse
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	if err := decoder.Decode(&payload); err != nil {
		return CPUTemperatureReading{}, fmt.Errorf("decode Redfish thermal response: %w", err)
	}
	maximum := float64(0)
	for _, sensor := range payload.Temperatures {
		contextName := strings.ToLower(strings.TrimSpace(sensor.PhysicalContext + " " + sensor.Name))
		if !cpuSensorLabel(contextName) || strings.EqualFold(sensor.Status.State, "Absent") {
			continue
		}
		if validTemperature(sensor.ReadingCelsius) && sensor.ReadingCelsius > maximum {
			maximum = sensor.ReadingCelsius
		}
	}
	if maximum == 0 {
		return CPUTemperatureReading{}, errors.New("the Redfish service returned no available CPU temperature sensor")
	}
	return CPUTemperatureReading{Celsius: maximum, Source: "redfish", Confidence: "medium"}, nil
}

type cachedTemperatureProvider struct {
	provider CPUTemperatureProvider
	mu       sync.Mutex
	reading  CPUTemperatureReading
	err      error
	at       time.Time
	cacheFor time.Duration
}

func (provider *cachedTemperatureProvider) ReadCPUTemperature(ctx context.Context) (CPUTemperatureReading, error) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if !provider.at.IsZero() && time.Since(provider.at) < provider.cacheFor {
		return provider.reading, provider.err
	}
	provider.reading, provider.err = provider.provider.ReadCPUTemperature(ctx)
	provider.at = time.Now()
	return provider.reading, provider.err
}

func configuredTemperatureProviders() []CPUTemperatureProvider {
	providers := make([]CPUTemperatureProvider, 0, 2)
	ipmi := configuredHostHealth()
	if ipmi.ipmi != "" && ipmi.ipmiError == "" {
		providers = append(providers, &cachedTemperatureProvider{provider: IPMITemperatureProvider{Command: ipmi.ipmi, Args: ipmi.ipmiArgs}, cacheFor: 15 * time.Second})
	}
	redfishURL := strings.TrimSpace(os.Getenv("XIRANG_REDFISH_THERMAL_URL"))
	redfishUser := strings.TrimSpace(os.Getenv("XIRANG_REDFISH_USERNAME"))
	passwordPath := strings.TrimSpace(os.Getenv("XIRANG_REDFISH_PASSWORD_FILE"))
	if redfishURL != "" && redfishUser != "" && passwordPath != "" {
		password, passwordErr := os.ReadFile(passwordPath)
		client, clientErr := redfishHTTPClient(strings.TrimSpace(os.Getenv("XIRANG_REDFISH_CA_FILE")))
		if passwordErr == nil && clientErr == nil && strings.TrimSpace(string(password)) != "" {
			providers = append(providers, &cachedTemperatureProvider{provider: RedfishTemperatureProvider{
				URL: redfishURL, Username: redfishUser, Password: strings.TrimSpace(string(password)), Client: client,
			}, cacheFor: 15 * time.Second})
		}
	}
	return providers
}

func redfishHTTPClient(caPath string) (*http.Client, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if caPath != "" {
		certificate, err := os.ReadFile(caPath)
		if err != nil {
			return nil, err
		}
		roots, err := x509.SystemCertPool()
		if err != nil || roots == nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(certificate) {
			return nil, errors.New("the Redfish CA file contains no certificates")
		}
		tlsConfig.RootCAs = roots
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsConfig
	return &http.Client{Transport: transport, Timeout: 4 * time.Second}, nil
}

func cpuSensorLabel(label string) bool {
	label = strings.ToLower(label)
	if strings.Contains(label, "ambient") || strings.Contains(label, "inlet") || strings.Contains(label, "exhaust") ||
		strings.Contains(label, "gpu") || strings.Contains(label, "nvme") || strings.Contains(label, "wifi") {
		return false
	}
	return strings.Contains(label, "cpu") || strings.Contains(label, "processor") || strings.Contains(label, "package") ||
		strings.Contains(label, "socket") || strings.Contains(label, "tdie") || strings.Contains(label, "tctl") || strings.Contains(label, "core")
}

func validTemperature(value float64) bool { return value > 0 && value < 250 }
