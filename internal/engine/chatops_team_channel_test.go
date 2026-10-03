package engine

import (
	"testing"

	"github.com/nixys/nxs-anomaly/internal/model"
	"github.com/nixys/nxs-anomaly/internal/store"
	"github.com/nixys/nxs-anomaly/internal/utils"
)

// teamChannelState is a team of two with one shared ChatOps channel per
// transport: a webhook-backed one and a Telegram group.
func teamChannelState() *store.State {
	state := store.NewState()
	for _, id := range []string{"u_a", "u_b"} {
		state.Users[id] = map[string]any{
			"id": id, "username": id, "name": id,
			"notification_targets": []any{map[string]any{"type": "log", "target": ""}},
		}
	}
	state.Teams["team_1"] = map[string]any{"id": "team_1", "name": "SRE", "member_ids": []any{"u_a", "u_b"}}
	state.ChatopsChannels["chat_hook"] = map[string]any{
		"id": "chat_hook", "platform": "slack", "name": "#sre", "team_id": "team_1",
		"notifications_enabled": true, "webhook_url": "https://chat.example.com/hooks/1",
	}
	state.ChatopsChannels["chat_tg"] = map[string]any{
		"id": "chat_tg", "platform": "telegram", "name": "-100123", "team_id": "team_1",
		"notifications_enabled": true,
	}
	return state
}

func teamChannelGroup() model.AlertGroup {
	return model.WrapAlertGroup(map[string]any{
		"id": "grp_1", "integration_id": "int_1", "title": "Boom",
		"severity": "critical", "status": "open", "episode_id": "epd_1",
	})
}

// chatNotifications counts the notifications addressed to each ChatOps
// channel, keyed by "<channel>:<target>".
func chatNotifications(state *store.State) map[string]int {
	out := map[string]int{}
	for _, rec := range state.Notifications {
		n, ok := rec.(model.Notification)
		if !ok {
			continue
		}
		if n.Channel() == "chatops" || (n.Channel() == "telegram" && n.Target() == "-100123") {
			out[n.Channel()+":"+n.Target()]++
		}
	}
	return out
}

// A step that pages a whole team reaches every member, and the team's channel
// belongs to each of them. The channel is one room, though: it must hear about
// the step once, not once per member.
func TestTeamChannelGetsOneMessagePerStep(t *testing.T) {
	state := teamChannelState()
	g := teamChannelGroup()
	e := honestyEngine(newMemStore(), DeliveryConfig{})

	e.notifyUsers(state, g, []string{"u_a", "u_b"}, "escalation step", utils.ToISO(utils.UTCNow()))

	got := chatNotifications(state)
	for _, key := range []string{"chatops:chat_hook", "telegram:-100123"} {
		if got[key] != 1 {
			t.Errorf("%s received %d notifications for one step, want 1 (all: %v)", key, got[key], got)
		}
	}
	outbound := map[string]int{}
	for _, msg := range state.ChatopsMessages {
		outbound[utils.StrVal(msg, "channel_id")]++
	}
	for _, ch := range []string{"chat_hook", "chat_tg"} {
		if outbound[ch] != 1 {
			t.Errorf("%s history has %d outbound rows for one step, want 1", ch, outbound[ch])
		}
	}
}

// The next step, a REPEAT or a restarted chain is a new page and reaches the
// channel again: the de-duplication is per step execution, not per group.
func TestTeamChannelHearsEachStepExecution(t *testing.T) {
	state := teamChannelState()
	g := teamChannelGroup()
	e := honestyEngine(newMemStore(), DeliveryConfig{})
	ts := utils.ToISO(utils.UTCNow())

	e.notifyUsers(state, g, []string{"u_a"}, "escalation step", ts)
	g.RestartEscalation(ts)
	e.notifyUsers(state, g, []string{"u_b"}, "escalation step", ts)

	if got := chatNotifications(state)["chatops:chat_hook"]; got != 2 {
		t.Errorf("channel received %d notifications over two step executions, want 2", got)
	}
}
