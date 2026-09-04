package main

import "testing"

func TestUnexpectedBlockedToolCall(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "groq tool choice rejection",
			body: `{"error":{"message":"litellm.BadRequestError: OpenAIException - Tool choice is none, but model called a tool."}}`,
			want: true,
		},
		{
			name: "ordinary bad request",
			body: `{"error":{"message":"invalid model"}}`,
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := unexpectedBlockedToolCall([]byte(tc.body)); got != tc.want {
				t.Fatalf("unexpectedBlockedToolCall()=%v want %v", got, tc.want)
			}
		})
	}
}
