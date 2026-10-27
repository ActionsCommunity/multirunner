package history

import (
	"errors"

	sqlite "modernc.org/sqlite"
)

const sqliteConstraint = 19

func isSQLiteConstraint(err error) bool {
	var sqliteError *sqlite.Error
	return errors.As(err, &sqliteError) &&
		sqliteError.Code()&0xff == sqliteConstraint
}
