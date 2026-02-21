package model

import "fmt"

type ExitCode int

const (
	ExitOK     ExitCode = 0
	ExitEnv    ExitCode = 10
	ExitRepo   ExitCode = 11
	ExitApply  ExitCode = 12
	ExitExport ExitCode = 13
	ExitWrite  ExitCode = 14
)

type PersonaError struct {
	Code ExitCode
	Op   string
	Err  error
}

func (e *PersonaError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("%s (exit %d)", e.Op, e.Code)
	}
	return fmt.Sprintf("%s: %v (exit %d)", e.Op, e.Err, e.Code)
}

func (e *PersonaError) Unwrap() error { return e.Err }

func Wrap(code ExitCode, op string, err error) error {
	if err == nil {
		return nil
	}
	return &PersonaError{Code: code, Op: op, Err: err}
}
