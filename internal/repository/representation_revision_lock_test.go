package repository

import (
	"regexp"

	"github.com/pashagolub/pgxmock/v4"
)

func expectRepresentationWriteGateShared(mock pgxmock.PgxPoolIface) {
	mock.ExpectExec(regexp.QuoteMeta(`SELECT lock_representation_write_gate_shared()`)).
		WillReturnResult(pgxmock.NewResult("SELECT", 1))
}

func expectRepresentationWriteGateExclusive(mock pgxmock.PgxPoolIface) {
	mock.ExpectExec(regexp.QuoteMeta(`SELECT lock_representation_write_gate_exclusive()`)).
		WillReturnResult(pgxmock.NewResult("SELECT", 1))
}

func expectLibraryFeedRevisionPrelock(mock pgxmock.PgxPoolIface) {
	mock.ExpectExec(regexp.QuoteMeta(`SELECT lock_library_feed_revisions()`)).
		WillReturnResult(pgxmock.NewResult("SELECT", 1))
}

func expectLibraryGlobalRevisionPrelock(mock pgxmock.PgxPoolIface) {
	mock.ExpectExec(regexp.QuoteMeta(`SELECT lock_library_global_revisions()`)).
		WillReturnResult(pgxmock.NewResult("SELECT", 1))
}
