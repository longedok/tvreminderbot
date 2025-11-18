package main

import "errors"

type UserError struct {
	Err error
	UserMsg string
}

func (e *UserError) Error() string {
	return e.Err.Error()
}

func (e *UserError) Unwrap() error {
	return e.Err
}

func NewUserError(internalErr error, userMsg string) *UserError {
	return &UserError{
		Err:     internalErr,
		UserMsg: userMsg,
	}
}

func getUserMessage(err error) string {
	var userErr *UserError
	if errors.As(err, &userErr) {
		return userErr.UserMsg
	}
	return err.Error()
}
