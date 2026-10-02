package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/codeagent"
	product "github.com/ww1489/seasprak/internal/errors"
)

// Mutating routes. Each handler decodes a strict DTO, resolves the single
// writer session through the catalog, and calls one existing session entry.

type contentBlock struct {
	Type       string `json:"type"`
	Text       string `json:"text,omitempty"`
	ArtifactID string `json:"artifactId,omitempty"`
}

type inputRequest struct {
	Kind          string         `json:"kind"`
	TargetTraceID string         `json:"targetTraceId,omitempty"`
	TargetAgent   string         `json:"targetAgent,omitempty"`
	Content       []contentBlock `json:"content"`
}

type inputReceiptDTO struct {
	InputID        string `json:"inputId"`
	TraceID        string `json:"traceId"`
	ActualKind     string `json:"actualKind"`
	TargetAgent    string `json:"targetAgent"`
	State          string `json:"state"`
	AcceptedCommit string `json:"acceptedCommit"`
}

type operationReceiptDTO struct {
	OperationID    string `json:"operationId"`
	State          string `json:"state"`
	Target         string `json:"target"`
	AcceptedCommit string `json:"acceptedCommit"`
	// Scope is "durable" or "instance"; instance receipts are lost on restart.
	Scope      string `json:"scope"`
	InstanceID string `json:"instanceId,omitempty"`
}

type operationStatusDTO struct {
	operationReceiptDTO
	Revision uint64 `json:"revision"`
	Result   string `json:"result,omitempty"`
	Error    string `json:"error,omitempty"`
}

type cancelRequest struct {
	ExpectedRevision *uint64 `json:"expectedRevision,omitempty"`
	Reason           string  `json:"reason,omitempty"`
}

type resumeRequest struct {
	ExpectedRevision uint64 `json:"expectedRevision"`
}

type continueRequest struct {
	TraceIDs         []string `json:"traceIds"`
	ExpectedRevision *uint64  `json:"expectedRevision,omitempty"`
}

type interactionRequest struct {
	Decision         string `json:"decision"`
	ExpectedRevision uint64 `json:"expectedRevision"`
	InstanceID       string `json:"instanceId"`
}

type reconcileRequest struct {
	InvocationID       string `json:"invocationId"`
	ToolCallID         string `json:"toolCallId"`
	ObservationID      string `json:"observationId"`
	ObservationVersion uint64 `json:"observationVersion,omitempty"`
	QueryID            string `json:"queryId,omitempty"`
	EvidenceRef        string `json:"evidenceRef,omitempty"`
	ExpectedRevision   uint64 `json:"expectedRevision"`
}

func (r *routes) mount(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/sessions/{sid}/open", r.openControl)
	mux.HandleFunc("POST /v1/sessions/{sid}/inputs", r.submitInput)
	mux.HandleFunc("GET /v1/sessions/{sid}/traces/{tid}", r.getTrace)
	mux.HandleFunc("POST /v1/sessions/{sid}/traces/{tid}/cancel", r.cancelTrace)
	mux.HandleFunc("POST /v1/sessions/{sid}/traces/{tid}/resume", r.resumeTrace)
	mux.HandleFunc("POST /v1/sessions/{sid}/traces/{tid}/reconcile", r.reconcile)
	mux.HandleFunc("POST /v1/sessions/{sid}/queue/continue", r.continueQueue)
	mux.HandleFunc("POST /v1/sessions/{sid}/interactions/{iid}/responses", r.respond)
	mux.HandleFunc("GET /v1/sessions/{sid}/operations/{oid}", r.getOperation)
	mux.HandleFunc("GET /v1/sessions/{sid}/events", r.events)
	mux.HandleFunc("GET /v1/sessions/{sid}/capabilities", r.capabilities)
	mux.HandleFunc("GET /v1/sessions/{sid}/render", r.render)
	mux.HandleFunc("GET /v1/sessions/{sid}/ui/events", r.uiEvents)
	r.mountHistory(mux)
	r.mountAttachments(mux)
	r.mountMaintenance(mux)
}

func idempotencyKey(w http.ResponseWriter, req *http.Request) (string, bool) {
	key := req.Header.Values("Idempotency-Key")
	if len(key) != 1 || key[0] == "" || len(key[0]) > 256 {
		WriteError(w, invalid("one Idempotency-Key header is required"))
		return "", false
	}
	return key[0], true
}

func (r *routes) writer(w http.ResponseWriter, req *http.Request) (*codeagent.AgentSession, bool) {
	s, err := r.catalog.Writer(req.Context(), req.PathValue("sid"))
	if err != nil {
		WriteError(w, err)
		return nil, false
	}
	return s, true
}

func (r *routes) durable(receipt codeagent.OperationReceipt) operationReceiptDTO {
	return operationReceiptDTO{OperationID: receipt.OperationID, State: receipt.State, Target: receipt.Target, AcceptedCommit: strconv.FormatUint(receipt.AcceptedCommit, 10), Scope: "durable"}
}

// acceptedOrError reports an accepted receipt even when post-acceptance work
// failed; that failure is visible through GET operation, not a lost receipt.
func (r *routes) acceptedOrError(w http.ResponseWriter, receipt codeagent.OperationReceipt, err error) {
	if receipt.OperationID != "" {
		writeJSON(w, http.StatusAccepted, r.durable(receipt))
		return
	}
	WriteError(w, err)
}

func (r *routes) submitInput(w http.ResponseWriter, req *http.Request) {
	key, ok := idempotencyKey(w, req)
	if !ok {
		return
	}
	var body inputRequest
	if err := DecodeJSON(w, req, &body); err != nil {
		WriteError(w, err)
		return
	}
	// Text blocks become one user message text; attachment blocks reference
	// saved session attachments by ID and are resolved by the session layer.
	var text strings.Builder
	var attachments []string
	if len(body.Content) == 0 {
		WriteError(w, invalid("content is required"))
		return
	}
	for _, b := range body.Content {
		switch {
		case b.Type == "text" && b.Text != "" && b.ArtifactID == "":
			if text.Len() > 0 {
				text.WriteString("\n")
			}
			text.WriteString(b.Text)
		case b.Type == "attachment" && b.ArtifactID != "" && b.Text == "":
			attachments = append(attachments, b.ArtifactID)
		default:
			WriteError(w, invalid("only non-empty text and attachment content blocks are supported"))
			return
		}
	}
	content, _ := json.Marshal(struct {
		Text        string   `json:"text"`
		Attachments []string `json:"attachments,omitempty"`
	}{text.String(), attachments})
	s, ok := r.writer(w, req)
	if !ok {
		return
	}
	receipt, err := s.SubmitInput(req.Context(), agent.InputCommand{Kind: body.Kind, TargetTraceID: body.TargetTraceID, TargetAgent: body.TargetAgent, Content: content, IdempotencyKey: key})
	if err != nil {
		WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, inputReceiptDTO{InputID: receipt.InputID, TraceID: receipt.TraceID, ActualKind: receipt.ActualKind, TargetAgent: receipt.TargetAgent.Name, State: receipt.State, AcceptedCommit: strconv.FormatUint(receipt.AcceptedCommit, 10)})
}

func (r *routes) getTrace(w http.ResponseWriter, req *http.Request) {
	snap, err := r.catalog.Snapshot(req.Context(), req.PathValue("sid"))
	if err != nil {
		WriteError(w, err)
		return
	}
	for _, tr := range projectSnapshot(snap).Traces {
		if tr.TraceID == req.PathValue("tid") {
			writeJSON(w, http.StatusOK, tr)
			return
		}
	}
	WriteError(w, product.NewError(product.CodeNotFound, "trace not found"))
}

func (r *routes) cancelTrace(w http.ResponseWriter, req *http.Request) {
	key, ok := idempotencyKey(w, req)
	if !ok {
		return
	}
	var body cancelRequest
	if err := DecodeJSON(w, req, &body); err != nil {
		WriteError(w, err)
		return
	}
	s, ok := r.writer(w, req)
	if !ok {
		return
	}
	// The worker is cancelled only after acceptance commits; a closed HTTP
	// connection never cancels the accepted operation.
	receipt, err := s.CancelTrace(context.WithoutCancel(req.Context()), codeagent.CancelTraceRequest{TraceID: req.PathValue("tid"), IdempotencyKey: key, ExpectedRevision: body.ExpectedRevision})
	r.acceptedOrError(w, receipt, err)
}

func (r *routes) resumeTrace(w http.ResponseWriter, req *http.Request) {
	key, ok := idempotencyKey(w, req)
	if !ok {
		return
	}
	var body resumeRequest
	if err := DecodeJSON(w, req, &body); err != nil {
		WriteError(w, err)
		return
	}
	s, ok := r.writer(w, req)
	if !ok {
		return
	}
	receipt, err := s.Resume(context.WithoutCancel(req.Context()), codeagent.ResumeCommand{TraceID: req.PathValue("tid"), ExpectedRevision: body.ExpectedRevision, IdempotencyKey: key})
	r.acceptedOrError(w, receipt, err)
}

func (r *routes) continueQueue(w http.ResponseWriter, req *http.Request) {
	key, ok := idempotencyKey(w, req)
	if !ok {
		return
	}
	var body continueRequest
	if err := DecodeJSON(w, req, &body); err != nil {
		WriteError(w, err)
		return
	}
	s, ok := r.writer(w, req)
	if !ok {
		return
	}
	receipt, err := s.ContinueQueued(context.WithoutCancel(req.Context()), codeagent.ContinueQueueRequest{TraceIDs: body.TraceIDs, IdempotencyKey: key, ExpectedRevision: body.ExpectedRevision})
	r.acceptedOrError(w, receipt, err)
}

func (r *routes) reconcile(w http.ResponseWriter, req *http.Request) {
	key, ok := idempotencyKey(w, req)
	if !ok {
		return
	}
	var body reconcileRequest
	if err := DecodeJSON(w, req, &body); err != nil {
		WriteError(w, err)
		return
	}
	s, ok := r.writer(w, req)
	if !ok {
		return
	}
	// The grant reference is resolved from the original frozen call by the
	// session layer; clients cannot supply it.
	receipt, err := s.Reconcile(context.WithoutCancel(req.Context()), codeagent.ReconcileCommand{TraceID: req.PathValue("tid"), InvocationID: body.InvocationID, CallID: body.ToolCallID, ObservationID: body.ObservationID, ObservationVersion: body.ObservationVersion, ExpectedRevision: body.ExpectedRevision, QueryID: body.QueryID, EvidenceRef: body.EvidenceRef, GrantRef: codeagent.OriginalGrantRef, IdempotencyKey: key})
	r.acceptedOrError(w, receipt, err)
}

func (r *routes) respond(w http.ResponseWriter, req *http.Request) {
	key, ok := idempotencyKey(w, req)
	if !ok {
		return
	}
	var body interactionRequest
	if err := DecodeJSON(w, req, &body); err != nil {
		WriteError(w, err)
		return
	}
	// Approval receipts are runtime-only. A response from another process
	// instance must not approve anything after restart.
	if body.InstanceID != r.catalog.InstanceID() {
		WriteError(w, product.NewError(product.CodeStateConflict, "approval belongs to another service instance"))
		return
	}
	switch body.Decision {
	case "allowed-once", "rejected", "cancelled":
	default:
		WriteError(w, invalid("decision is invalid"))
		return
	}
	s, ok := r.writer(w, req)
	if !ok {
		return
	}
	receipt, err := s.RespondInteraction(context.WithoutCancel(req.Context()), codeagent.InteractionResponse{InteractionID: req.PathValue("iid"), Decision: body.Decision, ExpectedRevision: body.ExpectedRevision, IdempotencyKey: key})
	if receipt.OperationID == "" {
		WriteError(w, err)
		return
	}
	out := r.durable(receipt)
	out.Scope, out.InstanceID = "instance", r.catalog.InstanceID()
	writeJSON(w, http.StatusAccepted, out)
}

type capabilitiesDTO struct {
	Agents []agent.AgentInfo `json:"agents"`
	Tools  []string          `json:"tools"`
	// Unavailable names routes this service deliberately does not provide.
	Unavailable []string `json:"unavailable"`
}

// capabilities browses without opening a writer.
func (r *routes) capabilities(w http.ResponseWriter, req *http.Request) {
	s, release, err := r.catalog.Browse(req.Context(), req.PathValue("sid"))
	if err != nil {
		WriteError(w, err)
		return
	}
	defer release()
	caps, err := s.Capabilities(req.Context())
	if err != nil {
		WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, capabilitiesDTO{Agents: caps.Agents, Tools: caps.Tools, Unavailable: []string{"host_shell", "resources_reload", "extension_commands", "workflow_import", "session_switch"}})
}

// openControl acquires the catalog's existing single writer without resuming
// work or answering approvals. Only this explicit POST changes control mode.
func (r *routes) openControl(w http.ResponseWriter, req *http.Request) {
	var body struct{}
	if err := DecodeJSON(w, req, &body); err != nil {
		WriteError(w, err)
		return
	}
	s, ok := r.writer(w, req)
	if !ok {
		return
	}
	snap, err := s.Snapshot(req.Context())
	if err != nil {
		WriteError(w, err)
		return
	}
	out := projectSnapshot(snap)
	out.InstanceID = r.catalog.InstanceID()
	writeJSON(w, http.StatusOK, out)
}

func (r *routes) getOperation(w http.ResponseWriter, req *http.Request) {
	s, release, err := r.catalog.Browse(req.Context(), req.PathValue("sid"))
	if err != nil {
		WriteError(w, err)
		return
	}
	defer release()
	status, err := s.GetOperation(req.Context(), req.PathValue("oid"))
	if err != nil {
		WriteError(w, err)
		return
	}
	out := operationStatusDTO{operationReceiptDTO: r.durable(status.OperationReceipt), Revision: status.Revision, Result: status.ResultRef, Error: status.ErrorRef}
	out.State = status.State
	writeJSON(w, http.StatusOK, out)
}
