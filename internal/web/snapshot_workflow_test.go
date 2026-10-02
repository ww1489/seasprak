package web

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/ww1489/seasprak/internal/codeagent"
)

func TestCodeSnapshotHasNoWorkflowNodeProjection(t *testing.T) {
	if _, exists := reflect.TypeFor[codeagent.Snapshot]().FieldByName("WorkflowNodes"); exists {
		t.Fatal("Code snapshot still owns workflow nodes")
	}
	raw, err := json.Marshal(projectSnapshot(emptySnapshot(3)))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "workflowNodes") || strings.Contains(string(raw), "nodeExecutionId") {
		t.Fatal("Code network snapshot retained embedded node projection")
	}
}
