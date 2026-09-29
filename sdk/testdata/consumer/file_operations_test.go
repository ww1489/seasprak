package consumer_test

import (
	"context"
	"testing"

	"github.com/ww1489/seasprak/sdk"
)

// An external backend must implement the complete port using only public types.
type publicFiles struct{}

func (publicFiles) List(context.Context, sdk.ListRequest) (sdk.ListResult, error) {
	return sdk.ListResult{}, nil
}
func (publicFiles) Read(context.Context, sdk.ReadRequest) (sdk.ReadResult, error) {
	return sdk.ReadResult{}, nil
}
func (publicFiles) Search(context.Context, sdk.SearchRequest) (sdk.SearchResult, error) {
	return sdk.SearchResult{}, nil
}
func (publicFiles) Write(context.Context, sdk.AuthorizedFileWrite) (sdk.FileEffect, error) {
	return sdk.FileEffect{SideEffect: "none"}, nil
}
func (publicFiles) Edit(context.Context, sdk.AuthorizedFileEdit) (sdk.FileEffect, error) {
	return sdk.FileEffect{SideEffect: "none"}, nil
}

func TestConsumerCanImplementFileOperations(t *testing.T) {
	var files sdk.FileOperations = publicFiles{}
	written, err := files.Write(t.Context(), sdk.AuthorizedFileWrite{})
	if err != nil || written.SideEffect != "none" {
		t.Fatal("public write effect unavailable")
	}
	edited, err := files.Edit(t.Context(), sdk.AuthorizedFileEdit{})
	if err != nil || edited.SideEffect != "none" {
		t.Fatal("public edit effect unavailable")
	}
}
