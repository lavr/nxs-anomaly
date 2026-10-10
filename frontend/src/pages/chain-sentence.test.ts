import { describe, expect, it } from 'vitest';

import type { EscalationStep } from '../api/types';
import { chainPagesNobody, stepReachesNobody, stepSentence, type ChainNames } from './chain-sentence';

const names: ChainNames = {
  user: (id) => id,
  team: (id) => id,
  schedule: (id) => id,
  channel: (id) => (id === 'chat-a' ? '#system-a' : id),
};
const t = (key: string, params?: Record<string, string | number>) =>
  params ? `${key}:${Object.values(params).join(',')}` : key;

describe('NOTIFY_CHATOPS_CHANNEL', () => {
  const step = { kind: 'NOTIFY_CHATOPS_CHANNEL', channel_id: 'chat-a' } as EscalationStep;
  const empty = { kind: 'NOTIFY_CHATOPS_CHANNEL' } as EscalationStep;

  it('names the channel it posts to', () => {
    expect(stepSentence(step, names, t)).toBe('chains.say.notifyChannel:#system-a');
    expect(stepSentence(empty, names, t)).toBe('chains.say.notifyChannelNobody');
  });

  it('reaches somebody only when a channel is chosen', () => {
    expect(stepReachesNobody(step)).toBe(false);
    expect(stepReachesNobody(empty)).toBe(true);
    expect(chainPagesNobody([step])).toBe(false);
  });

  it('falls back to the id when the caller resolves no channel names', () => {
    const { channel: _unused, ...withoutChannels } = names;
    expect(stepSentence(step, withoutChannels, t)).toBe('chains.say.notifyChannel:chat-a');
  });
});
