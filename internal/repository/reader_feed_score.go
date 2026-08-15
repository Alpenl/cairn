package repository

import (
	"fmt"
	"reflect"
	"slices"

	"webtag/internal/model"
)

const (
	readerFeedPendingConfirmationWeight = 100
	readerFeedSavedLibraryWeight        = 70
	readerFeedSubscriptionRecentWeight  = 40
	readerFeedUnreadWeight              = 20
	readerFeedReadLaterWeight           = 10
)

type readerFeedSignalContribution struct {
	code         model.ReaderFeedReasonCode
	enabled      bool
	contribution int
	params       model.ReaderFeedReasonParams
}

func scoreReaderFeedItem(item model.ReaderFeedItem) (model.ReaderFeedItem, error) {
	read := item.Read
	readLater := item.ReadLater
	createdAt := item.CreatedAt
	inbox, reading, subscription := "inbox", "reading", "subscription"

	signals := []readerFeedSignalContribution{
		{code: model.ReaderFeedReasonPendingConfirmation, enabled: item.Source == inbox, contribution: readerFeedPendingConfirmationWeight, params: model.ReaderFeedReasonParams{Source: &inbox}},
		{code: model.ReaderFeedReasonSavedLibrary, enabled: item.Source == reading, contribution: readerFeedSavedLibraryWeight, params: model.ReaderFeedReasonParams{Source: &reading}},
		{code: model.ReaderFeedReasonSubscriptionRecent, enabled: item.Source == subscription, contribution: readerFeedSubscriptionRecentWeight, params: model.ReaderFeedReasonParams{Source: &subscription}},
		{code: model.ReaderFeedReasonUnread, enabled: true, contribution: boolScore(!item.Read, readerFeedUnreadWeight), params: model.ReaderFeedReasonParams{Read: &read}},
		{code: model.ReaderFeedReasonReadLater, enabled: true, contribution: boolScore(item.ReadLater, readerFeedReadLaterWeight), params: model.ReaderFeedReasonParams{ReadLater: &readLater}},
		{code: model.ReaderFeedReasonChronologicalFallback, enabled: true, params: model.ReaderFeedReasonParams{CreatedAt: &createdAt}},
	}

	item.Score = 0
	item.ScoreContributions = model.ReaderFeedScoreContributions{}
	item.EnabledScoreSignals = make([]model.ReaderFeedReasonCode, 0, len(signals))
	for _, signal := range signals {
		setReaderFeedContribution(&item.ScoreContributions, signal.code, boolScore(signal.enabled, signal.contribution))
		if signal.enabled {
			item.Score += signal.contribution
			item.EnabledScoreSignals = append(item.EnabledScoreSignals, signal.code)
		}
	}

	winner, err := selectReaderFeedReason(signals)
	if err != nil {
		return model.ReaderFeedItem{}, err
	}
	item.ReasonCode = winner.code
	item.ReasonParams = winner.params
	item.ReasonContribution = winner.contribution
	item.ReasonText, err = readerFeedReasonText(winner.code, winner.params)
	if err != nil {
		return model.ReaderFeedItem{}, err
	}
	return item, nil
}

func scoreReaderFeedItems(items []model.ReaderFeedItem) ([]model.ReaderFeedItem, error) {
	scored := make([]model.ReaderFeedItem, 0, len(items))
	for _, item := range items {
		value, err := scoreReaderFeedItem(item)
		if err != nil {
			return nil, err
		}
		scored = append(scored, value)
	}
	return scored, nil
}

func ensureReaderFeedItemScore(item model.ReaderFeedItem) (model.ReaderFeedItem, error) {
	if item.EnabledScoreSignals == nil {
		return scoreReaderFeedItem(item)
	}
	if err := validateReaderFeedItemScore(item); err != nil {
		return model.ReaderFeedItem{}, err
	}
	return item, nil
}

func validateReaderFeedItemScore(item model.ReaderFeedItem) error {
	expected, err := scoreReaderFeedItem(item)
	if err != nil {
		return err
	}
	if item.Score != expected.Score || item.ScoreContributions != expected.ScoreContributions ||
		!slices.Equal(item.EnabledScoreSignals, expected.EnabledScoreSignals) ||
		item.ReasonCode != expected.ReasonCode || !reflect.DeepEqual(item.ReasonParams, expected.ReasonParams) ||
		item.ReasonContribution != expected.ReasonContribution || item.ReasonText != expected.ReasonText {
		return fmt.Errorf("%w: frozen score evidence is inconsistent", ErrInvalidReaderFeedReason)
	}
	return nil
}

func boolScore(enabled bool, score int) int {
	if enabled {
		return score
	}
	return 0
}

func setReaderFeedContribution(contributions *model.ReaderFeedScoreContributions, code model.ReaderFeedReasonCode, value int) {
	switch code {
	case model.ReaderFeedReasonPendingConfirmation:
		contributions.PendingConfirmation = value
	case model.ReaderFeedReasonSavedLibrary:
		contributions.SavedLibrary = value
	case model.ReaderFeedReasonSubscriptionRecent:
		contributions.SubscriptionRecent = value
	case model.ReaderFeedReasonUnread:
		contributions.Unread = value
	case model.ReaderFeedReasonReadLater:
		contributions.ReadLater = value
	case model.ReaderFeedReasonChronologicalFallback:
		contributions.ChronologicalFallback = value
	}
}

func selectReaderFeedReason(signals []readerFeedSignalContribution) (readerFeedSignalContribution, error) {
	var fallback *readerFeedSignalContribution
	var winner *readerFeedSignalContribution
	for index := range signals {
		signal := &signals[index]
		if signal.code == model.ReaderFeedReasonChronologicalFallback && signal.enabled {
			fallback = signal
		}
		if !signal.enabled || signal.contribution <= 0 {
			continue
		}
		if winner == nil || signal.contribution > winner.contribution ||
			(signal.contribution == winner.contribution && readerFeedReasonPriority(signal.code) < readerFeedReasonPriority(winner.code)) {
			winner = signal
		}
	}
	if winner == nil {
		winner = fallback
	}
	if winner == nil {
		return readerFeedSignalContribution{}, fmt.Errorf("%w: chronological fallback is disabled", ErrInvalidReaderFeedReason)
	}
	if _, err := readerFeedReasonText(winner.code, winner.params); err != nil {
		return readerFeedSignalContribution{}, err
	}
	return *winner, nil
}

func readerFeedReasonPriority(code model.ReaderFeedReasonCode) int {
	switch code {
	case model.ReaderFeedReasonPendingConfirmation:
		return 0
	case model.ReaderFeedReasonSavedLibrary:
		return 1
	case model.ReaderFeedReasonSubscriptionRecent:
		return 2
	case model.ReaderFeedReasonUnread:
		return 3
	case model.ReaderFeedReasonReadLater:
		return 4
	case model.ReaderFeedReasonChronologicalFallback:
		return 5
	default:
		return 6
	}
}

func readerFeedReasonText(code model.ReaderFeedReasonCode, params model.ReaderFeedReasonParams) (string, error) {
	if !readerFeedReasonParamsValid(code, params) {
		return "", fmt.Errorf("%w: reason %q is missing required params", ErrInvalidReaderFeedReason, code)
	}
	switch code {
	case model.ReaderFeedReasonPendingConfirmation:
		return "收件箱采集", nil
	case model.ReaderFeedReasonSavedLibrary:
		return "已保存到资料库", nil
	case model.ReaderFeedReasonSubscriptionRecent:
		return "订阅更新", nil
	case model.ReaderFeedReasonUnread:
		return "尚未阅读", nil
	case model.ReaderFeedReasonReadLater:
		return "已加入稍后读", nil
	case model.ReaderFeedReasonChronologicalFallback:
		return "按时间排序", nil
	default:
		return "", fmt.Errorf("%w: unsupported reason %q", ErrInvalidReaderFeedReason, code)
	}
}

func readerFeedReasonParamsValid(code model.ReaderFeedReasonCode, params model.ReaderFeedReasonParams) bool {
	switch code {
	case model.ReaderFeedReasonPendingConfirmation:
		return params.Source != nil && *params.Source == "inbox"
	case model.ReaderFeedReasonSavedLibrary:
		return params.Source != nil && *params.Source == "reading"
	case model.ReaderFeedReasonSubscriptionRecent:
		return params.Source != nil && *params.Source == "subscription"
	case model.ReaderFeedReasonUnread:
		return params.Read != nil && !*params.Read
	case model.ReaderFeedReasonReadLater:
		return params.ReadLater != nil && *params.ReadLater
	case model.ReaderFeedReasonChronologicalFallback:
		return params.CreatedAt != nil
	default:
		return false
	}
}
