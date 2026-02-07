package pkg

import "fmt"

var ErrorsEnums = struct {
	ErrModelsNotFound error
}{
	ErrModelsNotFound: fmt.Errorf("models not found"),
}
