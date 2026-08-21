package service

import (
	"webtag/internal/httperr"
	"webtag/internal/problem"
)

// readerTestStores wires only the feature interfaces a focused test double
// implements. It keeps production construction explicit while allowing each
// test fake to define just the methods exercised by that test.
func readerTestStores(store any) ReaderStores {
	var stores ReaderStores
	stores.Thoughts, _ = store.(ReaderThoughtStore)
	stores.Notes, _ = store.(ReaderNoteStore)
	stores.Inbox, _ = store.(ReaderInboxStore)
	stores.Todos, _ = store.(ReaderTodoStore)
	stores.Library, _ = store.(ReaderLibraryStore)
	stores.Hosts, _ = store.(ReaderHostStore)
	return stores
}

func problemHTTPStatus(err *problem.Error) int {
	carrier, ok := httperr.As(err)
	if !ok {
		return 0
	}
	return carrier.HTTPStatus()
}
