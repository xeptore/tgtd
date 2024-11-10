package must

import (
	"errors"
	"fmt"

	"github.com/xeptore/flaw/v8"
)

func BeFlaw(err error) *flaw.Flaw {
	if f := new(flaw.Flaw); errors.As(err, &f) {
		return f
	}
	panic(fmt.Sprintf("expected error to be of type *flaw.Flaw, got error of type %T: %v", err, err))
}
