package sindex

// Sentinel errors returned by the engine.
var (
	// ErrNotFound is returned when a primary key does not exist.
	ErrNotFound = newError("sindex: record not found")
	// ErrUniqueConflict is returned when a unique index constraint is violated.
	ErrUniqueConflict = newError("sindex: unique index conflict")
	// ErrExists is returned by Insert when the primary key already exists.
	ErrExists = newError("sindex: record already exists")
	// ErrIndexNotFound is returned when an index name is unknown.
	ErrIndexNotFound = newError("sindex: index not found")
	// ErrIndexExists is returned when creating an index that already exists.
	ErrIndexExists = newError("sindex: index already exists")
	// ErrInvalidArgument is returned for malformed definitions or options.
	ErrInvalidArgument = newError("sindex: invalid argument")
	// ErrTxnClosed is returned when a transaction is used after commit/rollback.
	ErrTxnClosed = newError("sindex: transaction already closed")
)

type engineError struct{ msg string }

func newError(msg string) error      { return &engineError{msg: msg} }
func (e *engineError) Error() string { return e.msg }

// UniqueConflictError carries details about a violated unique index.
type UniqueConflictError struct {
	Index       string
	Values      []Value
	ExistingKey []byte
}

func (e *UniqueConflictError) Error() string { return ErrUniqueConflict.Error() }
func (e *UniqueConflictError) Unwrap() error { return ErrUniqueConflict }
