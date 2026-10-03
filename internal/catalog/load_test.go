package catalog

import (
	"strings"
	"testing"
)

const minimalCatalog = `{
  "schema_version": 1,
  "generated_at": "2026-01-01T00:00:00Z",
  "generator": "test",
  "devices": [{"id": "board-a", "name": "Board A", "platform": "esp32s3"}],
  "projects": []
}`

func TestParseAcceptsKnownSchema(t *testing.T) {
	c, err := Parse([]byte(minimalCatalog))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(c.Devices) != 1 || c.Devices[0].ID != "board-a" {
		t.Fatalf("unexpected devices: %+v", c.Devices)
	}
}

// A generator that adds a field without bumping the schema version must not
// leave an older binary with a bare decode error: the operator needs to be
// told to upgrade. This is what happened when aliases was added.
func TestParseUnknownFieldAsksForUpgrade(t *testing.T) {
	cases := map[string]string{
		"top level": strings.Replace(minimalCatalog,
			`"generator": "test",`, `"generator": "test", "future_top": true,`, 1),
		"inside a device": strings.Replace(minimalCatalog,
			`"platform": "esp32s3"`, `"platform": "esp32s3", "future_device_field": ["x"]`, 1),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(data))
			if err == nil {
				t.Fatal("Parse accepted a catalog with an unknown field")
			}
			msg := err.Error()
			if !strings.Contains(msg, "meshflash upgrade") {
				t.Errorf("error does not tell the operator to upgrade: %q", msg)
			}
			if !strings.Contains(msg, "unknown field") {
				t.Errorf("error lost the name of the offending field: %q", msg)
			}
		})
	}
}

func TestParseNewerSchemaVersionAsksForUpgrade(t *testing.T) {
	data := strings.Replace(minimalCatalog, `"schema_version": 1`, `"schema_version": 2`, 1)
	_, err := Parse([]byte(data))
	if err == nil {
		t.Fatal("Parse accepted an unsupported schema version")
	}
	if !strings.Contains(err.Error(), "meshflash upgrade") {
		t.Errorf("error does not tell the operator to upgrade: %q", err)
	}
}

// A newer schema version is reported as such even when it also carries fields
// this build does not know, rather than as an unknown-field decode error.
func TestParseNewerSchemaWinsOverUnknownField(t *testing.T) {
	data := strings.Replace(minimalCatalog, `"schema_version": 1`, `"schema_version": 2, "future_top": true`, 1)
	_, err := Parse([]byte(data))
	if err == nil {
		t.Fatal("Parse accepted an unsupported schema version")
	}
	if !strings.Contains(err.Error(), "schema version 2") {
		t.Errorf("expected the schema version message, got %q", err)
	}
}

func TestParseMalformedJSONIsNotAnUpgradeProblem(t *testing.T) {
	_, err := Parse([]byte(`{"schema_version": 1,`))
	if err == nil {
		t.Fatal("Parse accepted malformed JSON")
	}
	if strings.Contains(err.Error(), "meshflash upgrade") {
		t.Errorf("malformed JSON should not suggest an upgrade: %q", err)
	}
}
