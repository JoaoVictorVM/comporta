package panel

import (
	"context"
	"strconv"
	"strings"

	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

// EventType is one type the events screen lists, with its label.
type EventType struct {
	Type  events.Type
	Label string
}

// EventTypes is the closed set of the events screen, in filter order.
var EventTypes = []EventType{
	{events.LoopCut, "Loop cut"},
	{events.Fallback, "Fallback"},
	{events.Blocked, "Blocked"},
	{events.Downgraded, "Downgraded"},
	{events.Warned, "Warned"},
	{events.Cached, "Cache hit"},
	{events.ConfigChanged, "Configuration changed"},
	{events.ConfigReloadFailed, "Configuration reload failed"},
	{events.Paused, "Pause"},
	{events.LimitStateUnavailable, "Limit state unavailable"},
	{events.LimitStateRecovered, "Limit state recovered"},
	{events.TranslationSkipped, "Translation skipped"},
}

func eventLabel(kind events.Type) (string, bool) {
	for _, t := range EventTypes {
		if t.Type == kind {
			return t.Label, true
		}
	}
	return "", false
}

// EventQuery is an events filter as submitted.
type EventQuery struct {
	App    string
	Type   string
	Cursor string
}

func (f EventFilters) canonical() string {
	return strings.Join([]string{"events", f.App, f.Type}, "\x00")
}

// Events lists events newest first, fifty per page.
func (s *Service) Events(ctx context.Context, q EventQuery) (EventList, error) {
	f := EventFilters{App: q.App, Type: q.Type}
	if f.App != "" && !storage.RecordName.MatchString(f.App) {
		return EventList{Invalid: true}, nil
	}
	types := make([]events.Type, 0, len(EventTypes))
	if f.Type != "" {
		if _, ok := eventLabel(events.Type(f.Type)); !ok {
			return EventList{Invalid: true}, nil
		}
		types = append(types, events.Type(f.Type))
	} else {
		for _, t := range EventTypes {
			types = append(types, t.Type)
		}
	}
	out := EventList{Filters: f}
	digest := f.canonical()
	var before *events.Cursor
	if q.Cursor != "" {
		c, ok := s.cursors.open(q.Cursor, "events", digest)
		if !ok {
			return EventList{Invalid: true}, nil
		}
		before = &events.Cursor{At: c.time(), ID: c.ID}
	}
	var page events.Page
	err := s.query(ctx, ScreenEvents, func(ctx context.Context) error {
		var err error
		page, err = s.store.PanelEvents(ctx, storage.PanelEventFilter{App: f.App, Types: types, Before: before, Limit: storage.PanelPageSize})
		return err
	})
	if err != nil {
		return out, err
	}
	for _, e := range page.Events {
		row, ok := formatEvent(e)
		if !ok {
			// A stored event outside the contract fails the fragment instead of
			// rendering what could not be validated.
			return EventList{Filters: f}, ErrUnavailable
		}
		out.Rows = append(out.Rows, row)
	}
	if page.Next != nil {
		out.Next = s.cursors.seal(cursor{Kind: "events", At: page.Next.At.UnixMicro(), ID: page.Next.ID}, digest)
	}
	return out, nil
}

// formatEvent projects each type through its own fields. The generic payload
// is never serialized to HTML.
func formatEvent(e events.Event) (EventRow, bool) {
	label, ok := eventLabel(e.Type)
	if !ok {
		return EventRow{}, false
	}
	row := EventRow{ID: e.ID, At: e.At, Type: string(e.Type), Label: label, App: e.App, Request: e.RequestID, Attempt: e.Payload.AttemptIndex}
	p := e.Payload
	add := func(name, value string) {
		if value != "" {
			row.Details = append(row.Details, name+": "+value)
		}
	}
	switch e.Type {
	case events.LoopCut:
		add("reason", p.Reason)
		if p.Count > 0 {
			add("similar requests", strconv.FormatInt(p.Count, 10))
		}
		if p.WindowMS > 0 {
			add("window", strconv.FormatInt(p.WindowMS, 10)+" ms")
		}
		if p.CooldownMS > 0 {
			add("cooldown", strconv.FormatInt(p.CooldownMS, 10)+" ms")
		}
		if e.UserHash != "" {
			row.User = ShortHash(e.UserHash)
		}
	case events.TranslationSkipped:
		add("model", p.Model)
		add("target", p.TargetFormat)
		add("feature", p.Feature)
	case events.Fallback:
		add("from", p.FromModel)
		add("to", p.ToModel)
		add("cause", p.Cause)
	case events.Blocked:
		add("code", p.ErrorCode)
		add("class", p.ErrorClass)
		add("reason", p.Reason)
	case events.Downgraded, events.Warned:
		add("limit", p.LimitName)
		add("scope", p.Scope)
		add("reason", p.Reason)
		add("requested", p.ModelRequested)
		add("used", p.ModelUsed)
	case events.Cached:
		add("outcome", p.Outcome)
	case events.ConfigChanged, events.ConfigReloadFailed:
		add("result", string(p.Result))
		if p.Generation > 0 {
			add("generation", strconv.FormatInt(p.Generation, 10))
		}
		for i, c := range p.Changes {
			if i == 20 {
				add("more changes", strconv.Itoa(len(p.Changes)-i))
				break
			}
			change := c.Path + ": " + masked(c.Old) + " -> " + masked(c.New)
			if c.SecretChanged {
				change = c.Path + ": secret changed"
			}
			row.Details = append(row.Details, change)
		}
	case events.Paused:
		if p.Paused != nil {
			state := "resumed"
			if *p.Paused {
				state = "paused"
			}
			add("state", state)
		}
		add("scope", p.Scope)
	case events.LimitStateUnavailable, events.LimitStateRecovered:
		add("severity", p.Severity)
		add("reason", p.Reason)
		if p.DegradedSeconds > 0 {
			add("degraded", strconv.FormatInt(p.DegradedSeconds, 10)+" s")
		}
	}
	return row, true
}

func masked(v *string) string {
	if v == nil {
		return "(absent)"
	}
	return *v
}
