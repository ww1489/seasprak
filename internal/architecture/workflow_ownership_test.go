package architecture

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type ownerSymbols struct {
	types  map[string]bool
	funcs  map[string]bool
	fields map[string]map[string]bool
}

func productionOwnerSymbols(t *testing.T, directory string) ownerSymbols {
	t.Helper()
	out := ownerSymbols{types: map[string]bool{}, funcs: map[string]bool{}, fields: map[string]map[string]bool{}}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(directory, entry.Name()), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			switch decl := decl.(type) {
			case *ast.FuncDecl:
				out.funcs[decl.Name.Name] = true
			case *ast.GenDecl:
				for _, spec := range decl.Specs {
					definition, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					out.types[definition.Name.Name] = true
					if structure, ok := definition.Type.(*ast.StructType); ok {
						fields := map[string]bool{}
						for _, field := range structure.Fields.List {
							for _, name := range field.Names {
								fields[name.Name] = true
							}
						}
						out.fields[definition.Name.Name] = fields
					}
				}
			}
		}
	}
	return out
}

func TestWorkflowDefinitionAndGraphHaveIndependentOwner(t *testing.T) {
	root := repoRoot()
	workflow := productionOwnerSymbols(t, filepath.Join(root, "internal", "workflowagent"))
	for _, name := range []string{"WorkflowDefinition", "WorkflowNode", "WorkflowValue", "WorkflowRef", "WorkflowCondition", "WorkflowEdge", "WorkflowNodeExecutor", "WorkflowBindings", "CompiledWorkflow"} {
		if !workflow.types[name] {
			t.Errorf("independent workflow owner is missing type %s", name)
		}
	}
	for _, name := range []string{"CompileWorkflow", "ValidateWorkflowInput", "BuildWorkflowGraph"} {
		if !workflow.funcs[name] {
			t.Errorf("independent workflow owner is missing function %s", name)
		}
	}
	for _, directory := range []string{"agent", filepath.Join("agent", "eino")} {
		lower := productionOwnerSymbols(t, filepath.Join(root, "internal", directory))
		for _, name := range []string{"WorkflowDefinition", "WorkflowNode", "WorkflowValue", "WorkflowRef", "WorkflowCondition", "WorkflowEdge", "WorkflowNodeExecutor", "CompiledWorkflow", "WorkflowBindings"} {
			if lower.types[name] {
				t.Errorf("lower layer %s still owns workflow type %s", directory, name)
			}
		}
		for _, name := range []string{"CompileWorkflow", "ValidateWorkflowInput", "BuildWorkflowGraph"} {
			if lower.funcs[name] {
				t.Errorf("lower layer %s still owns workflow function %s", directory, name)
			}
		}
	}
}

func TestCodeAgentHasNoEmbeddedWorkflowOrApplicationCatalog(t *testing.T) {
	root := repoRoot()
	for _, directory := range []string{"codeagent", filepath.Join("codeagent", "state")} {
		code := productionOwnerSymbols(t, filepath.Join(root, "internal", directory))
		for _, name := range []string{"WorkflowNodeRun", "workflowRun", "workflowStop", "Catalog", "CatalogCreateRequest"} {
			if code.types[name] {
				t.Errorf("Code owner %s still contains exited type %s", directory, name)
			}
		}
		for _, name := range []string{"CompileWorkflowTarget", "admitWorkflowInput", "workflowTarget", "executeWorkflow", "runChildWorkflow", "matchesWorkflowChild", "resumeWorkflow", "validateWorkflowResume", "commitWorkflowStop", "closeWorkflowCalls", "BeginWorkflowNode", "FinishWorkflowNode", "WorkflowNodeForCall", "WorkflowToolRetryable", "CommitWorkflowStop", "CommitWorkflowResume", "ClaimWorkflowApprovedTool"} {
			if code.funcs[name] {
				t.Errorf("Code owner %s still contains exited function %s", directory, name)
			}
		}
		for name, fields := range code.fields {
			for _, field := range []string{"WorkflowNodes", "workflowStop", "workflowFrom"} {
				if fields[field] {
					t.Errorf("Code owner %s.%s still contains exited field %s", directory, name, field)
				}
			}
		}
	}
	lower := productionOwnerSymbols(t, filepath.Join(root, "internal", "agent"))
	if lower.fields["AgentDefinition"]["Workflow"] {
		t.Error("ordinary AgentDefinition still carries an embedded workflow")
	}
}
