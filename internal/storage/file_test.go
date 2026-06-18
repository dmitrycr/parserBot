package storage

import "testing"

func TestValidateChat(t *testing.T) {
	tests := []struct {
		name    string
		chat    Chat
		wantErr bool
	}{
		{
			name: "channel",
			chat: Chat{
				ID:         1,
				Type:       "channel",
				AccessHash: 2,
			},
		},
		{
			name: "group without access hash",
			chat: Chat{
				ID:   1,
				Type: "group",
			},
		},
		{
			name: "missing id",
			chat: Chat{
				Type:       "channel",
				AccessHash: 2,
			},
			wantErr: true,
		},
		{
			name: "missing access hash",
			chat: Chat{
				ID:   1,
				Type: "channel",
			},
			wantErr: true,
		},
		{
			name: "unsupported type",
			chat: Chat{
				ID:         1,
				Type:       "unknown",
				AccessHash: 2,
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateChat(tt.chat)
			if tt.wantErr && err == nil {
				t.Fatal("expected error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
