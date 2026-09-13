package models

import "testing"

// StepSubject answers "what subject does this step actually send", which is
// not the column once a step replies in the contact's thread. Preflight, the
// content score and the test send all read it, and getting it wrong reports
// every follow-up as having no subject line (issue #472).
func TestStepSubject(t *testing.T) {
	steps := []Sequence{
		{Subject: "Quick question", Kind: "email", ThreadReply: true},
		{Subject: "", Kind: "email", ThreadReply: true},
		{Subject: "", Kind: "action", ThreadReply: true},
		{Subject: "", Kind: "email", ThreadReply: true},
		{Subject: "New angle", Kind: "email", ThreadReply: false},
		{Subject: "", Kind: "email", ThreadReply: true},
		{Subject: "Own subject", Kind: "email", ThreadReply: false},
	}
	for _, tc := range []struct {
		name string
		i    int
		want string
	}{
		{"the first step writes its own", 0, "Quick question"},
		{"a follow-up inherits the conversation's", 1, "Quick question"},
		{"an action node between steps is skipped", 3, "Quick question"},
		{"a step that starts a new thread keeps its own", 4, "New angle"},
		{"the steps after it inherit that one", 5, "New angle"},
		{"a step opting out writes its own", 6, "Own subject"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := StepSubject(steps, tc.i); got != tc.want {
				t.Errorf("StepSubject(%d) = %q, want %q", tc.i, got, tc.want)
			}
		})
	}
}

// A threading step with no earlier email to inherit from falls back to its
// own subject rather than sending a blank one. That is the branch-reached and
// the deleted-predecessor case, and it is also what the send path does when it
// finds no parent.
func TestStepSubjectFallsBackWhenThereIsNothingToInherit(t *testing.T) {
	steps := []Sequence{
		{Subject: "", Kind: "action", ThreadReply: true},
		{Subject: "Only subject", Kind: "email", ThreadReply: true},
	}
	if got := StepSubject(steps, 1); got != "Only subject" {
		t.Errorf("StepSubject = %q, want the step's own subject", got)
	}
}

// An index nobody has is not a panic.
func TestStepSubjectOutOfRange(t *testing.T) {
	if got := StepSubject(nil, 0); got != "" {
		t.Errorf("StepSubject(nil, 0) = %q, want empty", got)
	}
	if got := StepSubject([]Sequence{{Subject: "x"}}, 3); got != "" {
		t.Errorf("StepSubject(_, 3) = %q, want empty", got)
	}
}
