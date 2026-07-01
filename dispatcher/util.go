package dispatcher

import "database/sql"

// nullString wraps s as a valid sql.NullString, for building store.Event
// values (IssueID/RunID are sql.NullString) from plain strings the
// dispatcher always has in hand.
func nullString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}
