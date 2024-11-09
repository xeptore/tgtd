package errutil

import (
	"fmt"
)

func UnknownError(err error) string {
	return fmt.Sprintf("unknown error of type %T received: %v", err, err)
}
