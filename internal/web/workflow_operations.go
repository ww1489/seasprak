package web

import (
	"context"
	"net/http"
	"strings"

	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/workflowagent"
)

type workflowOperationReceiptDTO struct {
	OperationID    string `json:"operationId"`
	State          string `json:"state"`
	Target         string `json:"target"`
	AcceptedCommit uint64 `json:"acceptedCommit"`
	Scope          string `json:"scope"`
	InstanceID     string `json:"instanceId,omitempty"`
}
type workflowRevisionBody struct {
	ExpectedRevision *uint64 `json:"expectedRevision"`
}
type workflowCancelBody struct {
	ExpectedRevision *uint64 `json:"expectedRevision"`
	Reason           string  `json:"reason,omitempty"`
}
type workflowInteractionBody struct {
	Decision         string  `json:"decision"`
	ExpectedRevision *uint64 `json:"expectedRevision"`
	InstanceID       string  `json:"instanceId"`
}

func workflowRevision(value *uint64) error {
	if value == nil || *value > (1<<53)-1 {
		return invalid("a safe expectedRevision is required")
	}
	return nil
}
func projectWorkflowReceipt(receipt workflowagent.WorkflowOperationReceipt, rid, iid, instance string) (workflowOperationReceiptDTO, error) {
	if receipt.OperationID == "" || receipt.AcceptedCommit > (1<<53)-1 {
		return workflowOperationReceiptDTO{}, product.NewError(product.CodeInternal, "workflow receipt cannot be projected")
	}
	switch receipt.ReceiptScope {
	case "durable":
		if receipt.Target != rid || receipt.AcceptedCommit == 0 || receipt.InstanceID != "" {
			return workflowOperationReceiptDTO{}, product.NewError(product.CodeInternal, "workflow receipt root differs")
		}
	case "instance":
		if receipt.Target != iid || iid == "" || receipt.AcceptedCommit != 0 || receipt.InstanceID != instance {
			return workflowOperationReceiptDTO{}, product.NewError(product.CodeInternal, "workflow approval receipt differs")
		}
	default:
		return workflowOperationReceiptDTO{}, product.NewError(product.CodeInternal, "workflow receipt scope differs")
	}
	return workflowOperationReceiptDTO{receipt.OperationID, receipt.State, receipt.Target, receipt.AcceptedCommit, receipt.ReceiptScope, receipt.InstanceID}, nil
}
func (r *routes) workflowControl(w http.ResponseWriter, req *http.Request) {
	key, ok := idempotencyKey(w, req)
	if !ok {
		return
	}
	action := req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:]
	var body workflowCancelBody
	if action == "cancel" {
		if err := DecodeJSON(w, req, &body); err != nil {
			WriteError(w, err)
			return
		}
	} else {
		var revision workflowRevisionBody
		if err := DecodeJSON(w, req, &revision); err != nil {
			WriteError(w, err)
			return
		}
		body.ExpectedRevision = revision.ExpectedRevision
	}
	if err := workflowRevision(body.ExpectedRevision); err != nil {
		WriteError(w, err)
		return
	}
	rid := req.PathValue("rid")
	run, err := r.workflows.Writer(req.Context(), rid)
	if err != nil {
		WriteError(w, err)
		return
	}
	command := workflowagent.WorkflowControlCommand{IdempotencyKey: key, ExpectedRevision: body.ExpectedRevision, Reason: body.Reason, Principal: r.principal}
	ctx := context.WithoutCancel(req.Context())
	var receipt workflowagent.WorkflowOperationReceipt
	switch action {
	case "pause":
		receipt, err = run.Pause(ctx, command)
	case "cancel":
		receipt, err = run.Cancel(ctx, command)
	case "resume":
		receipt, err = run.Resume(ctx, command)
	}
	if receipt.OperationID == "" {
		WriteError(w, err)
		return
	}
	dto, err := projectWorkflowReceipt(receipt, rid, "", "")
	if err != nil {
		WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, dto)
}
func (r *routes) workflowRespond(w http.ResponseWriter, req *http.Request) {
	key, ok := idempotencyKey(w, req)
	if !ok {
		return
	}
	var body workflowInteractionBody
	if err := DecodeJSON(w, req, &body); err != nil {
		WriteError(w, err)
		return
	}
	if err := workflowRevision(body.ExpectedRevision); err != nil {
		WriteError(w, err)
		return
	}
	if body.InstanceID == "" {
		WriteError(w, invalid("instanceId is required"))
		return
	}
	switch body.Decision {
	case "allowed-once", "rejected", "cancelled":
	default:
		WriteError(w, invalid("decision is invalid"))
		return
	}
	rid, iid := req.PathValue("rid"), req.PathValue("iid")
	run, err := r.workflows.Writer(req.Context(), rid)
	if err != nil {
		WriteError(w, err)
		return
	}
	snapshot, err := run.Snapshot(req.Context())
	if err != nil {
		WriteError(w, err)
		return
	}
	if snapshot.RunID != rid || snapshot.InstanceID != body.InstanceID {
		WriteError(w, product.NewError(product.CodeStateConflict, "approval belongs to another workflow instance"))
		return
	}
	question, found := snapshot.Interactions[iid]
	if !found || question.ID != iid || question.InstanceID != body.InstanceID {
		WriteError(w, product.NewError(product.CodeNotFound, "interaction is unavailable in this instance"))
		return
	}
	// A completed answer may only return the upper layer's original keyed
	// receipt, even after its tool is claimed. RespondInteraction still checks
	// the key and decision before any new authorization or state mutation.
	answered := false
	for _, operation := range snapshot.Operations {
		if operation.Receipt.ReceiptScope == "instance" && operation.Receipt.Target == iid && operation.Receipt.InstanceID == body.InstanceID {
			answered = true
			break
		}
	}
	if !answered && (question.State != "ready" || snapshot.State != "paused" || !snapshot.ExecutionStopped) {
		WriteError(w, product.NewError(product.CodeStateConflict, "interaction is not ready"))
		return
	}
	receipt, err := run.RespondInteraction(context.WithoutCancel(req.Context()), workflowagent.WorkflowInteractionResponse{InteractionID: iid, Decision: body.Decision, ExpectedRevision: *body.ExpectedRevision, IdempotencyKey: key, Principal: r.principal})
	if receipt.OperationID == "" {
		WriteError(w, err)
		return
	}
	dto, err := projectWorkflowReceipt(receipt, rid, iid, body.InstanceID)
	if err != nil {
		WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, dto)
}
func (r *routes) workflowOperation(w http.ResponseWriter, req *http.Request) {
	snapshot, err := r.workflows.Snapshot(req.Context(), req.PathValue("rid"))
	if err != nil {
		WriteError(w, err)
		return
	}
	if snapshot.Revision > (1<<53)-1 {
		WriteError(w, product.NewError(product.CodeInternal, "workflow revision cannot be projected"))
		return
	}
	operation, found := snapshot.Operations[req.PathValue("oid")]
	if !found {
		WriteError(w, product.NewError(product.CodeNotFound, "workflow operation does not exist"))
		return
	}
	out := struct {
		OperationID string `json:"operationId"`
		State       string `json:"state"`
		Revision    uint64 `json:"revision"`
	}{operation.Receipt.OperationID, operation.State, snapshot.Revision}
	writeJSON(w, http.StatusOK, out)
}
