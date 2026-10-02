package web

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestWorkflowHTTPExactVersionAndCanonicalInputRetries(t *testing.T) {
	conf := testConfig(t)
	v1, v2 := httpLiteralWorkflow(), httpLiteralWorkflow()
	v2.Version = "v2"
	configureHTTPWorkflow(t, conf, v1, v2)
	server, token, fake := workflowHTTPServer(t, conf)
	status, first := call(t, server, token, "POST", "/v1/workflow-runs", "same", workflowCreateBody(conf))
	if status != 202 {
		t.Fatalf("versioned create status=%d", status)
	}
	body := workflowCreateBody(conf)
	body["input"] = json.RawMessage(` { "name" : "one" } `)
	status, again := call(t, server, token, "POST", "/v1/workflow-runs", "same", body)
	if status != 202 || !reflect.DeepEqual(first, again) {
		t.Fatal("equivalent canonical object changed creation receipt")
	}
	body["version"] = "v2"
	status, out := call(t, server, token, "POST", "/v1/workflow-runs", "same", body)
	assertWorkflowError(t, status, out, 409, "idempotency_conflict")
	entries, err := os.ReadDir(filepath.Join(conf.StateRoot, "workflow-runs"))
	if err != nil || len(entries) != 1 || fake.Calls() != 0 {
		t.Fatal("version conflict materialized another run or invoked Code")
	}
	status, second := call(t, server, token, "POST", "/v1/workflow-runs", "new-version", body)
	if status != 202 || second["definitionVersion"] != "v2" || second["runId"] == first["runId"] {
		t.Fatalf("exact independent version unavailable: %d %v", status, second)
	}
}

func TestWorkflowHTTPUnavailableProfileReservesNothing(t *testing.T) {
	conf := testConfig(t)
	configureHTTPWorkflow(t, conf, httpLiteralWorkflow())
	setProfile(t, conf, "")
	server, token, fake := workflowHTTPServer(t, conf)
	status, out := call(t, server, token, "POST", "/v1/workflow-runs", "profile", workflowCreateBody(conf))
	assertWorkflowError(t, status, out, 503, "resource_unavailable")
	entries, err := os.ReadDir(filepath.Join(conf.StateRoot, "catalog", "creations"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".json" {
			t.Fatal("unavailable profile reserved a creation")
		}
	}
	if _, err := os.Lstat(filepath.Join(conf.StateRoot, "workflow-runs")); !os.IsNotExist(err) || fake.Calls() != 0 {
		t.Fatal("unavailable profile materialized or executed")
	}
}
