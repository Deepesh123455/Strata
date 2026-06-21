package resp

import (
	"bytes"
	"errors"
	"fmt"
)

// ErrIncomplete tells our TCP worker: "The network packet got split in half, 
// go back to sleep and wait for the rest of the bytes."
var ErrIncomplete = errors.New("incomplete command, waiting for more bytes")

// parseLen converts a byte slice (e.g., "123" or "-1") into an integer.
// It returns an error if the field is empty, contains non-digits, or overflows.
func parseLen(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, errors.New("empty length field")
	}

	neg := false
	if b[0] == '-' {
		neg = true
		b = b[1:]
	}

	if len(b) == 0 {
		return 0, errors.New("invalid negative sign only")
	}

	val := 0
	for _, char := range b {
		if char < '0' || char > '9' {
			return 0, fmt.Errorf("invalid digit %q in length", char)
		}
		nextVal := val*10 + int(char-'0')
		if nextVal < val { // Check for integer overflow
			return 0, errors.New("integer overflow in length")
		}
		val = nextVal
	}

	if neg {
		return -val, nil
	}
	return val, nil
}

// Parse extracts zero-allocation slices from the raw network buffer.
// It returns a list of byte slices (the command), how many bytes it consumed, and an error.
func Parse(buffer []byte) ([][]byte, int, error) {
	cursor := 0
	var args [][]byte

	// 1. Validate the start of a RESP Array (e.g., "*3\r\n")
	if len(buffer) == 0 || buffer[cursor] != '*' {
		return nil, 0, errors.New("protocol error: expected '*'")
	}
	cursor++ // Move past the '*'

	// 2. Find the '\r' to isolate the number of expected arguments
	rIdx := bytes.IndexByte(buffer[cursor:], '\r')
	if rIdx == -1 {
		return nil, 0, ErrIncomplete
	}

	argCount, err := parseLen(buffer[cursor : cursor+rIdx])
	if err != nil {
		return nil, 0, fmt.Errorf("protocol error: invalid argument count: %w", err)
	}
	
	// Move the cursor past the number and the "\r\n"
	cursor += rIdx + 2

	if argCount < 0 {
		if argCount == -1 {
			return nil, cursor, nil // Null array
		}
		return nil, 0, fmt.Errorf("protocol error: invalid negative argument count %d", argCount)
	}

	// 3. Loop through and extract each argument using Slices
	for i := 0; i < argCount; i++ {
		// Ensure we are looking at a Bulk String prefix (e.g., "$3\r\n")
		if cursor >= len(buffer) {
			return nil, 0, ErrIncomplete
		}
		if buffer[cursor] != '$' {
			return nil, 0, fmt.Errorf("protocol error: expected '$', got %q", buffer[cursor])
		}
		cursor++

		// Find the length of the upcoming word
		rIdx = bytes.IndexByte(buffer[cursor:], '\r')
		if rIdx == -1 {
			return nil, 0, ErrIncomplete
		}
		
		strLen, err := parseLen(buffer[cursor : cursor+rIdx])
		if err != nil {
			return nil, 0, fmt.Errorf("protocol error: invalid bulk string length: %w", err)
		}
		cursor += rIdx + 2

		// Null Bulk String check ($-1\r\n)
		if strLen < 0 {
			if strLen == -1 {
				args = append(args, nil)
				continue
			}
			return nil, 0, fmt.Errorf("protocol error: invalid negative bulk string length %d", strLen)
		}

		// Security Check: Do we actually have enough bytes left in the buffer?
		// We need strLen + 2 bytes for the bulk string content and trailing \r\n
		if cursor+strLen+2 > len(buffer) {
			return nil, 0, ErrIncomplete
		}

		// Validate that the bulk string ends with CRLF
		if buffer[cursor+strLen] != '\r' || buffer[cursor+strLen+1] != '\n' {
			return nil, 0, errors.New("protocol error: bulk string not terminated with CRLF")
		}

		// --- THE MAGIC: PLACING THE WINDOW FRAME ---
		// We create a slice pointing directly to the word in the original buffer.
		// Zero new memory is allocated!
		wordSlice := buffer[cursor : cursor+strLen]
		args = append(args, wordSlice)

		// Move cursor past the word and its trailing "\r\n"
		cursor += strLen + 2
	}

	// Return the extracted slices, and tell the worker exactly where we stopped reading.
	return args, cursor, nil
}