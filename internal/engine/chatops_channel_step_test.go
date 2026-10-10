package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nixys/nxs-anomaly/internal/model"
	"github.com/nixys/nxs-anomaly/internal/store"
	"github.com/nixys/nxs-anomaly/internal/utils"
)

// systemState is two systems, A and B, each with a team and a room, and one
// person — u1 — on both teams. That person is what makes membership fan-out
// carry one system's alert into the other's room.
func systemState(steps []any, fanout bool) *store.State {
	s := newState(steps)
	s.Teams["team-a"] = map[string]any{"id": "team-a", "name": "A", "member_ids": []any{"u1"}}
	s.Teams["team-b"] = map[string]any{"id": "team-b", "name": "B", "member_ids": []any{"u1"}}
	for _, id := range []string{"a", "b"} {
		s.ChatopsChannels["chat-"+id] = map[string]any{
			"id": "chat-" + id, "name": "#" + id, "platform": "mattermost", "team_id": "team-" + id,
			"notifications_enabled": true, "membership_fanout": fanout,
			"webhook_url": "https://chat.example.com/hooks/" + id,
		}
	}
	return s
}

// channelPosts counts the alert messages queued for each ChatOps channel.
func channelPosts(s *store.State) map[string]int {
	out := map[string]int{}
	for _, rec := range s.Notifications {
		n := rec.(model.Notification)
		if n.Channel() == "chatops" {
			out[n.Target()]++
		}
	}
	return out
}

// The step posts to the room it names even when the system's team is empty:
// with membership fan-out an alert for a team of nobody reached no room.
func TestChatopsChannelStepPostsWithoutMembers(t *testing.T) {
	e := newEngine()
	s := systemState([]any{map[string]any{"kind": StepNotifyChatopsChannel, "channel_id": "chat-a"}}, true)
	s.Teams["team-a"]["member_ids"] = []any{}
	g := model.WrapAlertGroup(newGroup(0, 0))

	e.advanceGroupLocked(s, g, "2026-05-10T10:00:00+00:00")

	if got := channelPosts(s); got["chat-a"] != 1 || got["chat-b"] != 0 {
		t.Errorf("posts = %v, want one to chat-a only", got)
	}
	refs := g.NotifiedChatChannels()
	if len(refs) != 1 || refs[0].ID != "chat-a" {
		t.Errorf("group records %+v, want chat-a, so its status changes reach the room", refs)
	}
}

// With fan-out off, an alert of system A reaches A's room through the step
// and nowhere else — not B's room, although the paged person is on B's team.
func TestChatopsChannelStepKeepsAlertsInTheirRoom(t *testing.T) {
	e := newEngine()
	s := systemState([]any{
		map[string]any{"kind": StepNotifyChatopsChannel, "channel_id": "chat-a"},
		map[string]any{"kind": StepNotifyTeam, "team_id": "team-a"},
	}, false)
	g := model.WrapAlertGroup(newGroup(0, 0))

	e.advanceGroupLocked(s, g, "2026-05-10T10:00:00+00:00")

	if got := channelPosts(s); got["chat-a"] != 1 || got["chat-b"] != 0 {
		t.Errorf("posts = %v, want chat-a once and chat-b never", got)
	}
}

// Fan-out stays the default: a channel that did not opt out still hears about
// its members' pages, so existing installations see no change.
func TestMembershipFanoutStaysTheDefault(t *testing.T) {
	e := newEngine()
	s := systemState([]any{map[string]any{"kind": StepNotifyTeam, "team_id": "team-a"}}, true)
	delete(s.ChatopsChannels["chat-a"], "membership_fanout")
	delete(s.ChatopsChannels["chat-b"], "membership_fanout")
	g := model.WrapAlertGroup(newGroup(0, 0))

	e.advanceGroupLocked(s, g, "2026-05-10T10:00:00+00:00")

	if got := channelPosts(s); got["chat-a"] != 1 || got["chat-b"] != 1 {
		t.Errorf("posts = %v, want both rooms of u1's teams, as before", got)
	}
}

// A channel reached by the step and by fan-out in the same step execution is
// posted to once: both paths share one key.
func TestChatopsChannelStepAndFanoutShareTheKey(t *testing.T) {
	e := newEngine()
	s := systemState(nil, true)
	g := model.WrapAlertGroup(newGroup(0, 0))
	seen := notificationIdemSet(s)
	ts := "2026-05-10T10:00:00+00:00"

	e.postToChatopsChannel(s, g, s.ChatopsChannels["chat-a"], nil, "step", ts, seen)
	e.fanoutChatopsNotifications(s, g, s.Users["u1"], "team", ts, seen)

	if got := channelPosts(s)["chat-a"]; got != 1 {
		t.Errorf("chat-a got %d posts in one step execution, want 1", got)
	}
}

func TestChatopsChannelStepSkipsMissingAndMutedChannels(t *testing.T) {
	e := newEngine()
	s := systemState([]any{
		map[string]any{"kind": StepNotifyChatopsChannel, "channel_id": "chat-gone"},
		map[string]any{"kind": StepNotifyChatopsChannel, "channel_id": "chat-b"},
	}, false)
	s.ChatopsChannels["chat-b"]["notifications_enabled"] = false
	g := model.WrapAlertGroup(newGroup(0, 0))

	e.advanceGroupLocked(s, g, "2026-05-10T10:00:00+00:00")

	if got := channelPosts(s); len(got) != 0 {
		t.Errorf("posts = %v, want none", got)
	}
	raw := g.Raw()
	if !logsContainType(raw, "chatops_channel_missing") || !logsContainType(raw, "chatops_channel_muted") {
		t.Errorf("the group's log does not say why no room was told: %v", raw["logs"])
	}
}

func TestChatopsChannelStepValidation(t *testing.T) {
	ms := newMemStore()
	ms.seed("chatops_channels", map[string]any{"id": "chat-a", "name": "#a", "platform": "slack"})
	e := crudEngine(ms)
	ctx := context.Background()

	if _, err := e.sanitizeStep(ctx, map[string]any{"kind": StepNotifyChatopsChannel}, 0); !errors.Is(err, ErrValidation) {
		t.Errorf("a step without channel_id: %v, want a validation error", err)
	}
	if _, err := e.sanitizeStep(ctx, map[string]any{"kind": StepNotifyChatopsChannel, "channel_id": "chat-x"}, 0); err == nil {
		t.Error("a step naming an unknown channel was accepted")
	}
	step, err := e.sanitizeStep(ctx, map[string]any{"kind": "notify_chatops_channel", "channel_id": "chat-a"}, 0)
	if err != nil || utils.StrVal(step, "channel_id") != "chat-a" || step["kind"] != StepNotifyChatopsChannel {
		t.Errorf("step = %v, %v", step, err)
	}
}

// A channel a chain posts to cannot be deleted from under it.
func TestChatopsChannelInUseCannotBeDeleted(t *testing.T) {
	ms := newMemStore()
	ms.seed("chatops_channels", map[string]any{"id": "chat-a", "name": "#a", "platform": "slack"})
	ms.seed("escalation_chains", map[string]any{"id": "chain-1", "name": "system a", "steps": []any{
		map[string]any{"kind": StepNotifyChatopsChannel, "channel_id": "chat-a"},
	}})
	e := crudEngine(ms)

	_, err := e.DeleteEntity(adminCtx(), "chatops_channels", "chat-a")
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "system a") {
		t.Errorf("delete = %v, want a conflict naming the chain", err)
	}
}

// The flag round-trips through the channel API, and is on unless switched off.
func TestMembershipFanoutOnChannelAPI(t *testing.T) {
	ms := newMemStore()
	e := crudEngine(ms)
	ch, err := e.CreateChatopsChannel(adminCtx(), map[string]any{"platform": "slack", "name": "#a"})
	if err != nil {
		t.Fatal(err)
	}
	if ch["membership_fanout"] != true {
		t.Errorf("membership_fanout = %v on a new channel, want true", ch["membership_fanout"])
	}
	ch, err = e.UpdateChatopsChannel(adminCtx(), utils.StrVal(ch, "id"), map[string]any{"membership_fanout": false})
	if err != nil || ch["membership_fanout"] != false {
		t.Errorf("after update: %v, %v", ch["membership_fanout"], err)
	}
}
