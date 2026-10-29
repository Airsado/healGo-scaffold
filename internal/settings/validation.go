package settings

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"unicode"
)

const maxValueBytes = 64 * 1024

var keyPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)

func validKey(key string) bool { return len(key) <= 128 && keyPattern.MatchString(key) }

func validActor(actor string) bool {
	// Audit identities come from the authenticated principal, not a request
	// field. Reject control characters to keep exported audit records readable.
	if actor == "" || actor != strings.TrimSpace(actor) || len(actor) > 128 {
		return false
	}
	for _, char := range actor {
		if unicode.IsControl(char) {
			return false
		}
	}
	return true
}

func normalized(value json.RawMessage) (json.RawMessage, error) {
	// A fixed byte budget applies before decoding to bound allocation and SQL
	// payload sizes. A registry validator can impose a smaller domain-specific
	// limit without changing the generic settings storage contract.
	if len(value) == 0 || len(value) > maxValueBytes || !json.Valid(value) {
		return nil, ErrInvalidInput
	}
	// Preserve decimal tokens exactly: a settings service must not round a
	// large integer through float64 merely because its schema is generic JSON.
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, ErrInvalidInput
	}
	canonical, err := json.Marshal(decoded)
	// JSON escaping can enlarge a valid input. Enforce the same budget on the
	// bytes that will actually be stored, so the next read cannot reject them.
	if err != nil || len(canonical) > maxValueBytes {
		return nil, ErrInvalidInput
	}
	return canonical, nil
}

func valueType(value json.RawMessage) byte {
	// Only normalized, nonempty JSON reaches this helper. The first byte is
	// sufficient for its top-level kind; numeric precision stays in json.Number.
	switch value[0] {
	case '"', '{', '[':
		return value[0]
	case 't', 'f':
		return 'b'
	case 'n':
		return 'n'
	default:
		return 'd'
	}
}

func validate(def Definition, value json.RawMessage, allowClear bool) (json.RawMessage, error) {
	value, err := normalized(value)
	if err != nil {
		return nil, ErrInvalidInput
	}
	// Only a JSON null write to a secret means clear. A missing HTTP value
	// must be rejected by the controller; an empty string is an explicit value.
	if def.Secret && allowClear && bytes.Equal(value, []byte("null")) {
		value = json.RawMessage(`""`)
	}
	if valueType(value) != valueType(def.Default) || valueType(value) == 'n' {
		// A typo such as "false" for a boolean must not silently change type.
		// Null defaults are excluded because they cannot establish a type contract.
		return nil, ErrInvalidInput
	}
	if def.Validate != nil {
		// Custom validators may include user input in their errors. Keep that
		// detail inside the application boundary instead of reflecting secrets.
		if err := def.Validate(append(json.RawMessage(nil), value...)); err != nil {
			return nil, ErrInvalidInput
		}
	}
	return value, nil
}
