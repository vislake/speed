package aigateway

import "testing"

func TestChatRequest_Validate_EmptyModel_Refused(t *testing.T) {
	req := ChatRequest{Messages: []ChatMessage{{Role: RoleUser, Content: "hi"}}}
	if got, ok := apperrCode(req.validate()); !ok || got != ErrEmptyModel.Code {
		t.Fatalf("validate() = %v, want ErrEmptyModel", req.validate())
	}
}

func TestChatRequest_Validate_NoMessages_Refused(t *testing.T) {
	req := ChatRequest{Model: "chat:default"}
	if got, ok := apperrCode(req.validate()); !ok || got != ErrEmptyMessages.Code {
		t.Fatalf("validate() = %v, want ErrEmptyMessages", req.validate())
	}
}

func TestChatRequest_Validate_EmptyMessageContent_Refused(t *testing.T) {
	req := ChatRequest{Model: "chat:default", Messages: []ChatMessage{{Role: RoleUser, Content: ""}}}
	if got, ok := apperrCode(req.validate()); !ok || got != ErrEmptyMessages.Code {
		t.Fatalf("validate() = %v, want ErrEmptyMessages", req.validate())
	}
}

func TestChatRequest_Validate_WellFormed_Accepted(t *testing.T) {
	req := ChatRequest{Model: "chat:default", Messages: []ChatMessage{{Role: RoleUser, Content: "hi"}}}
	if err := req.validate(); err != nil {
		t.Fatalf("validate() = %v, want nil", err)
	}
}
