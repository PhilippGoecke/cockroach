// Copyright 2021 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package tracingui

import (
	"sort"
	"time"

	"github.com/cockroachdb/cockroach/pkg/util/tracing"
	"github.com/cockroachdb/cockroach/pkg/util/tracing/tracingpb"
)

// This file has helpers for the RPCs done by the #/debug/tracez page, which
// lists a snapshot of the spans in the Tracer's active spans registry.

// ProcessSnapshot massages a trace snapshot to prepare it for presentation in
// the UI.
func ProcessSnapshot(snapshot tracing.SpansSnapshot) *ProcessedSnapshot {
	// Flatten the recordings.
	spans := make([]tracingpb.RecordedSpan, 0, len(snapshot.Traces)*3)
	for _, r := range snapshot.Traces {
		spans = append(spans, r...)
	}

	spansMap := make(map[uint64]*processedSpan)
	childrenMap := make(map[uint64][]*processedSpan)
	processedSpans := make([]processedSpan, len(spans))
	for i, s := range spans {
		p := processSpan(s, snapshot)
		ptr := &processedSpans[i]
		*ptr = p
		spansMap[p.SpanID] = &processedSpans[i]
		if _, ok := childrenMap[p.ParentSpanID]; !ok {
			childrenMap[p.ParentSpanID] = []*processedSpan{&processedSpans[i]}
		} else {
			childrenMap[p.ParentSpanID] = append(childrenMap[p.ParentSpanID], &processedSpans[i])
		}
	}
	// Propagate tags up.
	for _, s := range processedSpans {
		for _, t := range s.Tags {
			if !t.PropagateUp || t.CopiedFromChild {
				continue
			}
			propagateTagUpwards(t, &s, spansMap)
		}
	}
	// Propagate tags down.
	for _, s := range processedSpans {
		for _, t := range s.Tags {
			if !t.Inherit || t.Inherited {
				continue
			}
			propagateInheritTagDownwards(t, &s, childrenMap)
		}
	}

	// Copy the stack traces and augment the map.
	stacks := make(map[int]string, len(snapshot.Stacks))
	for k, v := range snapshot.Stacks {
		stacks[k] = v
	}
	// Fill in messages for the goroutines for which we don't have a stack trace.
	for _, s := range spans {
		gid := int(s.GoroutineID)
		if _, ok := stacks[gid]; !ok {
			stacks[gid] = "Goroutine not found. Goroutine must have finished since the span was created."
		}
	}
	return &ProcessedSnapshot{
		Spans:  processedSpans,
		Stacks: stacks,
	}
}

// ProcessedSnapshot represents a snapshot of open tracing spans plus stack
// traces for all the goroutines.
type ProcessedSnapshot struct {
	Spans []processedSpan
	// Stacks contains stack traces for the goroutines referenced by the Spans
	// through their GoroutineID field.
	Stacks map[int]string // GoroutineID to stack trace
}

var hiddenTags = map[string]struct{}{
	"_unfinished": {},
	"_verbose":    {},
	"_dropped":    {},
	"node":        {},
	"store":       {},
}

type processedSpan struct {
	Operation                     string
	TraceID, SpanID, ParentSpanID uint64
	Start                         time.Time
	GoroutineID                   uint64
	Tags                          []ProcessedTag
}

// ProcessedTag is a span tag that was processed and expanded by processTag.
type ProcessedTag struct {
	Key, Val string
	Caption  string
	Link     string
	Hidden   bool
	// highlight is set if the tag should be rendered with a little exclamation
	// mark.
	Highlight bool

	// inherit is set if this tag should be passed down to children, and
	// recursively.
	Inherit bool
	// inherited is set if this tag was passed over from an ancestor.
	Inherited bool

	PropagateUp bool
	// copiedFromChild is set if this tag did not originate on the owner span, but
	// instead was propagated upwards from a child span.
	CopiedFromChild bool
}

// propagateTagUpwards copies tag from sp to all of sp's ancestors.
func propagateTagUpwards(tag ProcessedTag, sp *processedSpan, spans map[uint64]*processedSpan) {
	tag.CopiedFromChild = true
	tag.Inherit = false
	parentID := sp.ParentSpanID
	for {
		p, ok := spans[parentID]
		if !ok {
			return
		}
		p.Tags = append(p.Tags, tag)
		parentID = p.ParentSpanID
	}
}

func propagateInheritTagDownwards(
	tag ProcessedTag, sp *processedSpan, children map[uint64][]*processedSpan,
) {
	tag.PropagateUp = false
	tag.Inherited = true
	tag.Hidden = true
	for _, child := range children[sp.SpanID] {
		child.Tags = append(child.Tags, tag)
		propagateInheritTagDownwards(tag, child, children)
	}
}

// processSpan massages a span for presentation in the UI. Some of the tags are
// expanded.
func processSpan(s tracingpb.RecordedSpan, snap tracing.SpansSnapshot) processedSpan {
	p := processedSpan{
		Operation:    s.Operation,
		TraceID:      uint64(s.TraceID),
		SpanID:       uint64(s.SpanID),
		ParentSpanID: uint64(s.ParentSpanID),
		Start:        s.StartTime,
		GoroutineID:  s.GoroutineID,
	}

	// Sort the tags.
	tagKeys := make([]string, 0, len(s.Tags))
	for k := range s.Tags {
		tagKeys = append(tagKeys, k)
	}
	sort.Strings(tagKeys)

	p.Tags = make([]ProcessedTag, len(s.Tags))
	for i, k := range tagKeys {
		p.Tags[i] = processTag(k, s.Tags[k], snap)
	}
	return p
}

// processTag massages span tags for presentation in the UI. It marks some tags
// as hidden, it marks some tags to be inherited by child spans, and it expands
// lock contention tags with information about the lock holder txn.
func processTag(k, v string, snap tracing.SpansSnapshot) ProcessedTag {
	p := ProcessedTag{
		Key: k,
		Val: v,
	}
	_, hidden := hiddenTags[k]
	p.Hidden = hidden

	switch k {
	case "lock_holder_txn":
		txnID := v
		// Take only the first 8 bytes, to keep the text shorter.
		txnIDShort := v[:8]
		p.Val = txnIDShort
		p.PropagateUp = true
		p.Highlight = true
		p.Link = txnIDShort
		txnState := findTxnState(txnID, snap)
		if !txnState.found {
			p.Caption = "blocked on unknown transaction"
		} else if txnState.curQuery != "" {
			p.Caption = "blocked on txn currently running query: " + txnState.curQuery
		} else {
			p.Caption = "blocked on idle txn"
		}
	case "statement":
		p.Inherit = true
		p.PropagateUp = true
	}

	return p
}

// txnState represents the current state of a SQL txn, as determined by
// findTxnState. Namely, the state contains the current SQL query running inside
// the transaction, if any.
type txnState struct {
	// found is set if any tracing spans pertaining to this transaction are found.
	found    bool
	curQuery string
}

// findTxnState looks through a snapshot for span pertaining to the specified
// transaction and, within those, looks for a running SQL query.
func findTxnState(txnID string, snap tracing.SpansSnapshot) txnState {
	// Iterate through all the traces and look for a "sql txn" span for the
	// respective transaction.
	for _, t := range snap.Traces {
		for _, s := range t {
			if s.Operation != "sql txn" || s.Tags["txn"] != txnID {
				continue
			}
			// I've found the transaction. Look through its children and find a SQL query.
			for _, s2 := range t {
				if s2.Operation == "sql query" {
					return txnState{
						found:    true,
						curQuery: s2.Tags["statement"],
					}
				}
			}
			return txnState{found: true}
		}
	}
	return txnState{found: false}
}
