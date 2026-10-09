// Package applimit decides how many apps a person may own (R-244, issue
// #131).
//
// Three places may say: the person's own account, their groups, and host
// policy. The account wins whatever the groups say, so an administrator can
// make an exception for one person in either direction. Otherwise the most
// generous group wins, unlimited beating any number, because being in a group
// should only ever give somebody more. Otherwise host policy. Zero is
// unlimited wherever it is set; somebody who may not create apps at all is
// somebody without app.create.
package applimit

import (
	"context"
	"fmt"

	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

// Where a limit comes from.
const (
	SourceUser   = "user"
	SourceGroup  = "group"
	SourcePolicy = "policy"
)

// Limit is the limit in force for one person, and how close they are to it.
type Limit struct {
	// Limit is how many apps they may own. 0 is unlimited.
	Limit int `json:"limit"`
	// Source is where it comes from: user, group or policy.
	Source string `json:"source"`
	// GroupID and GroupName name the group, when Source is group.
	GroupID   string `json:"group_id,omitempty"`
	GroupName string `json:"group_name,omitempty"`

	// Owned is how many apps they own now.
	Owned int `json:"owned"`

	// MaxApps is the value set on their own account: null when none is.
	MaxApps *int `json:"max_apps"`
}

// Store reads what a limit is decided from.
type Store interface {
	Facts(ctx context.Context, userID string) (state.LimitFacts, error)
}

// Policy reads host policy.
type Policy interface {
	Load(ctx context.Context) (policy.Document, error)
}

// Service decides limits.
type Service struct {
	Store  Store
	Policy Policy
}

// For is the limit in force for userID.
func (s *Service) For(ctx context.Context, userID string) (Limit, error) {
	doc, err := s.Policy.Load(ctx)
	if err != nil {
		return Limit{}, err
	}
	facts, err := s.Store.Facts(ctx, userID)
	if err != nil {
		return Limit{}, err
	}
	return Resolve(doc, facts), nil
}

// Resolve applies R-244's order to the facts.
func Resolve(doc policy.Document, f state.LimitFacts) Limit {
	l := Limit{Owned: f.Owned, MaxApps: f.UserMaxApps}
	switch {
	case f.UserMaxApps != nil:
		l.Limit, l.Source = *f.UserMaxApps, SourceUser
	case len(f.Groups) > 0:
		l.Source = SourceGroup
		best := f.Groups[0]
		for _, g := range f.Groups[1:] {
			if best.MaxApps == 0 {
				break
			}
			if g.MaxApps == 0 || g.MaxApps > best.MaxApps {
				best = g
			}
		}
		l.Limit, l.GroupID, l.GroupName = best.MaxApps, best.GroupID, best.Name
	default:
		l.Limit, l.Source = doc.MaxAppsPerUser, SourcePolicy
	}
	return l
}

// Refusal is the error for a create past the limit (R-105): the limit, where
// it comes from, and who can raise it. owned is what the create counted.
func Refusal(l Limit, owned int) error {
	var where string
	switch l.Source {
	case SourceUser:
		where = "set on your account"
	case SourceGroup:
		where = fmt.Sprintf("set for the %s group", l.GroupName)
	default:
		where = "this installation sets for each person"
	}
	e := errs.Newf(errs.PolicyAppLimitReached,
		"You own %d %s, and %d is the limit %s, so Pando did not create this one.",
		owned, plural(owned), l.Limit, where).
		WithRemedy("Ask an administrator to raise the limit, or delete an app you no longer need.").
		WithDetail("limit", l.Limit).
		WithDetail("owned", owned).
		WithDetail("source", l.Source)
	if l.GroupID != "" {
		e = e.WithDetail("group_id", l.GroupID)
	}
	return e
}

func plural(n int) string {
	if n == 1 {
		return "app"
	}
	return "apps"
}
