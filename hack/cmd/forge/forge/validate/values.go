package validate

import (
	"fmt"
)

func NilOrEmpty(s *string, label string) error {
	if s == nil || *s == "" {
		return fmt.Errorf("%q is nil or empty string", label)
	}

	return nil
}

func Empty(s, label string) error {
	if s == "" {
		return fmt.Errorf("%q is empty string", label)
	}

	return nil
}
