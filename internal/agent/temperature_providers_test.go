package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseIPMITemperatureUsesOnlyCPUSensors(t *testing.T) {
	output := "CPU1 Temp | 47 degrees C | ok\nSystem Temp | 29 degrees C | ok\nNVMe Temp | 68 degrees C | ok\nProcessor 2 | 51 degrees C | ok\n"
	if value := parseIPMICPUTemperature(output); value != 51 {
		t.Fatalf("unexpected IPMI CPU temperature: %.1f", value)
	}
}

func TestRedfishProviderUsesHealthyCPUPhysicalContext(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		username, password, ok := request.BasicAuth()
		if !ok || username != "monitor" || password != "secret" {
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"Temperatures":[{"Name":"CPU 1","PhysicalContext":"CPU","ReadingCelsius":54,"Status":{"Health":"OK","State":"Enabled"}},{"Name":"Inlet","PhysicalContext":"Intake","ReadingCelsius":21,"Status":{"Health":"OK","State":"Enabled"}},{"Name":"CPU bad","PhysicalContext":"CPU","ReadingCelsius":99,"Status":{"Health":"Critical","State":"Enabled"}}]}`))
	}))
	defer server.Close()
	provider := RedfishTemperatureProvider{URL: server.URL, Username: "monitor", Password: "secret", Client: server.Client()}
	reading, err := provider.ReadCPUTemperature(context.Background())
	if err != nil || reading.Celsius != 54 || reading.Source != "redfish" || reading.Confidence != "medium" {
		t.Fatalf("unexpected Redfish reading: %#v err=%v", reading, err)
	}
}
