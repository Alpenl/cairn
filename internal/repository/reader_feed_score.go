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
	code         model.ReaderFeedScoreSignal
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
		{code: model.ReaderFeedSignalPendingConfirmation, enabled: item.Source == inbox, contribution: readerFeedPendingConfirmationWeight, params: model.ReaderFeedReasonParams{Source: &inbox}},
		{code: model.ReaderFeedSignalSavedLibrary, enabled: item.Source == reading, contribution: readerFeedSavedLibraryWeight, params: model.ReaderFeedReasonParams{Source: &reading}},
		{code: model.ReaderFeedSignalSubscriptionRecent, enabled: item.Source == subscription, contribution: readerFeedSubscriptionRecentWeight, params: model.ReaderFeedReasonParams{Source: &subscription}},
		{code: model.ReaderFeedSignalUnread, enabled: true, contribution: boolScore(!item.Read, readerFeedUnreadWeight), params: model.ReaderFeedReasonParams{Read: &read}},
		{code: model.ReaderFeedSignalReadLater, enabled: true, contribution: boolScore(item.ReadLater, readerFeedReadLaterWeight), params: model.ReaderFeedReasonParams{ReadLater: &readLater}},
		{code: model.ReaderFeedSignalChronologicalFallback, enabled: true, params: model.ReaderFeedReasonParams{CreatedAt: &createdAt}},
	}

	item.Score = 0
	item.ScoreContributions = model.ReaderFeedScoreContributions{}
	item.EnabledScoreSignals = make([]model.ReaderFeedScoreSignal, 0, len(signals))
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
	item.ReasonCode = winner.code.ReasonCode()
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

func setReaderFeedContribution(contributions *model.ReaderFeedScoreContributions, signal model.ReaderFeedScoreSignal, value int) {
	switch signal {
	case model.ReaderFeedSignalPendingConfirmation:
		contributions.PendingConfirmation = value
	case model.ReaderFeedSignalSavedLibrary:
		contributions.SavedLibrary = value
	case model.ReaderFeedSignalSubscriptionRecent:
		contributions.SubscriptionRecent = value
	case model.ReaderFeedSignalUnread:
		contributions.Unread = value
	case model.ReaderFeedSignalReadLater:
		contributions.ReadLater = value
	case model.ReaderFeedSignalChronologicalFallback:
		contributions.ChronologicalFallback = value
	}
}

func selectReaderFeedReason(signals []readerFeedSignalContribution) (readerFeedSignalContribution, error) {
	var fallback *readerFeedSignalContribution
	var winner *readerFeedSignalContribution
	for index := range signals {
		signal := &signals[index]
		if signal.code == model.ReaderFeedSignalChronologicalFallback && signal.enabled {
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

func readerFeedReasonPriority(signal model.ReaderFeedScoreSignal) int {
	switch signal {
	case model.ReaderFeedSignalPendingConfirmation:
		return 0
	case model.ReaderFeedSignalSavedLibrary:
		return 1
	case model.ReaderFeedSignalSubscriptionRecent:
		return 2
	case model.ReaderFeedSignalUnread:
		return 3
	case model.ReaderFeedSignalReadLater:
		return 4
	case model.ReaderFeedSignalChronologicalFallback:
		return 5
	default:
		return 6
	}
}

// readerFeedReasonText covers the scored reasons only. Reasons outside the
// ranking pass, such as Home's continue_reading, own their own wording.
func readerFeedReasonText(signal model.ReaderFeedScoreSignal, params model.ReaderFeedReasonParams) (string, error) {
	if !readerFeedReasonParamsValid(signal, params) {
		return "", fmt.Errorf("%w: reason %q is missing required params", ErrInvalidReaderFeedReason, signal)
	}
	switch signal {
	case model.ReaderFeedSignalPendingConfirmation:
		return "收件箱采集", nil
	case model.ReaderFeedSignalSavedLibrary:
		return "已保存到资料库", nil
	case model.ReaderFeedSignalSubscriptionRecent:
		return "订阅更新", nil
	case model.ReaderFeedSignalUnread:
		return "尚未阅读", nil
	case model.ReaderFeedSignalReadLater:
		return "已加入稍后读", nil
	case model.ReaderFeedSignalChronologicalFallback:
		return "按时间排序", nil
	default:
		return "", fmt.Errorf("%w: unsupported reason %q", ErrInvalidReaderFeedReason, signal)
	}
}

func readerFeedReasonParamsValid(signal model.ReaderFeedScoreSignal, params model.ReaderFeedReasonParams) bool {
	switch signal {
	case model.ReaderFeedSignalPendingConfirmation:
		return params.Source != nil && *params.Source == "inbox"
	case model.ReaderFeedSignalSavedLibrary:
		return params.Source != nil && *params.Source == "reading"
	case model.ReaderFeedSignalSubscriptionRecent:
		return params.Source != nil && *params.Source == "subscription"
	case model.ReaderFeedSignalUnread:
		return params.Read != nil && !*params.Read
	case model.ReaderFeedSignalReadLater:
		return params.ReadLater != nil && *params.ReadLater
	case model.ReaderFeedSignalChronologicalFallback:
		return params.CreatedAt != nil
	default:
		return false
	}
}
