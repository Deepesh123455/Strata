package tests

import (
	"bytes"
	"errors"
	"testing"

	"github.com/Deepesh123455/Redis-Cache/DataPlane/internal/resp"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name         string
		input        []byte
		wantArgs     [][]byte
		wantConsumed int
		wantErr      error
	}{
		{
			name:         "Standard valid command",
			input:        []byte("*3\r\n$3\r\nSET\r\n$4\r\nkey1\r\n$5\r\nvalue\r\n"),
			wantArgs:     [][]byte{[]byte("SET"), []byte("key1"), []byte("value")},
			wantConsumed: 34,
			wantErr:      nil,
		},
		{
			name:         "Null bulk string ($-1)",
			input:        []byte("*2\r\n$3\r\nGET\r\n$-1\r\n"),
			wantArgs:     [][]byte{[]byte("GET"), nil},
			wantConsumed: 18,
			wantErr:      nil,
		},
		{
			name:         "Incomplete arguments count",
			input:        []byte("*3\r\n$3\r\nSET\r\n"),
			wantArgs:     nil,
			wantConsumed: 0,
			wantErr:      resp.ErrIncomplete,
		},
		{
			name:         "Incomplete bulk string payload",
			input:        []byte("*3\r\n$3\r\nSET\r\n$4\r\nkey"),
			wantArgs:     nil,
			wantConsumed: 0,
			wantErr:      resp.ErrIncomplete,
		},
		{
			name:         "Empty input",
			input:        []byte(""),
			wantArgs:     nil,
			wantConsumed: 0,
			wantErr:      errors.New("protocol error"),
		},
		{
			name:         "Invalid type prefix (no *)",
			input:        []byte("SET key val\r\n"),
			wantArgs:     nil,
			wantConsumed: 0,
			wantErr:      errors.New("protocol error"),
		},
		{
			name:         "Non-digit length",
			input:        []byte("*3\r\n$X\r\n"),
			wantArgs:     nil,
			wantConsumed: 0,
			wantErr:      errors.New("protocol error"),
		},
		{
			name:         "Invalid negative bulk string length (<-1)",
			input:        []byte("*2\r\n$3\r\nGET\r\n$-5\r\n"),
			wantArgs:     nil,
			wantConsumed: 0,
			wantErr:      errors.New("protocol error"),
		},
		{
			name:         "Missing trailing CRLF for bulk string",
			input:        []byte("*2\r\n$3\r\nGET\r\n$4\r\nkeys"),
			wantArgs:     nil,
			wantConsumed: 0,
			wantErr:      resp.ErrIncomplete,
		},
		{
			name:         "Incorrect CRLF trailing characters",
			input:        []byte("*2\r\n$3\r\nGET\r\n$4\r\nkeysXX"),
			wantArgs:     nil,
			wantConsumed: 0,
			wantErr:      errors.New("protocol error"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args, consumed, err := resp.Parse(tt.input)

			if tt.wantErr != nil {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if tt.wantErr == resp.ErrIncomplete {
					if !errors.Is(err, resp.ErrIncomplete) {
						t.Errorf("expected ErrIncomplete, got %v", err)
					}
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if consumed != tt.wantConsumed {
					t.Errorf("expected consumed %d, got %d", tt.wantConsumed, consumed)
				}
				if len(args) != len(tt.wantArgs) {
					t.Fatalf("expected %d args, got %d", len(tt.wantArgs), len(args))
				}
				for i := range args {
					if !bytes.Equal(args[i], tt.wantArgs[i]) {
						t.Errorf("arg %d: expected %q, got %q", i, tt.wantArgs[i], args[i])
					}
				}
			}
		})
	}
}
