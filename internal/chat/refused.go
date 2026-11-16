package chat

import "errors"

// refusal is the chat system's answer to a request it will not take, however
// often it is made.
type refusal struct{ err error }

func (r refusal) Error() string { return r.err.Error() }
func (r refusal) Unwrap() error { return r.err }

// Refused marks err as a refusal: the chat system was reached and said no, as
// to a reaction it already holds. A driver returns it for those errors of its
// calls; any other error of a call is taken for the chat system being out of
// reach, and the call is made again.
func Refused(err error) error {
	if err == nil {
		return nil
	}
	return refusal{err}
}

// IsRefused says whether err is a refusal.
func IsRefused(err error) bool {
	var r refusal
	return errors.As(err, &r)
}
