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

// readerTestFeatureSet is test-only composition sugar. Production routes use
// ReaderApplications' named fields directly; focused tests can still exercise
// one feature without building unrelated fakes.
type readerTestFeatureSet struct {
	*ReaderThoughtApplication
	*ReaderNoteApplication
	*ReaderInboxApplication
	*ReaderTodoApplication
	*ReaderLibraryApplication
	*ReaderHostApplication
}

func newReaderTestFeatureSet(stores ReaderStores, ai ReaderAIBackend, options ...ReaderApplicationOptions) *readerTestFeatureSet {
	var configured ReaderApplicationOptions
	if len(options) > 0 {
		configured = options[0]
	}
	if configured.InboxConfirmCommands == nil {
		configured.InboxConfirmCommands, _ = stores.Inbox.(ReaderInboxConfirmCommands)
	}
	if configured.InboxBulkConfirmCommands == nil {
		configured.InboxBulkConfirmCommands, _ = stores.Inbox.(ReaderInboxBulkConfirmCommands)
	}
	if configured.InboxAIConfirmCommands == nil {
		configured.InboxAIConfirmCommands, _ = stores.Inbox.(ReaderInboxAIConfirmCommands)
	}
	if configured.FeedFeedbackCommands == nil {
		configured.FeedFeedbackCommands, _ = stores.Library.(ReaderFeedFeedbackCommands)
	}
	if configured.HostRestoreCommands == nil {
		configured.HostRestoreCommands, _ = stores.Hosts.(ReaderHostRestoreCommands)
	}
	applications := NewReaderApplications(stores, ai, configured)
	return &readerTestFeatureSet{
		ReaderThoughtApplication: applications.Thoughts,
		ReaderNoteApplication:    applications.Notes,
		ReaderInboxApplication:   applications.Inbox,
		ReaderTodoApplication:    applications.Todos,
		ReaderLibraryApplication: applications.Library,
		ReaderHostApplication:    applications.Hosts,
	}
}

func problemHTTPStatus(err *problem.Error) int {
	carrier, ok := httperr.As(err)
	if !ok {
		return 0
	}
	return carrier.HTTPStatus()
}
