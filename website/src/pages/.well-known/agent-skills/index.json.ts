import type { APIRoute } from 'astro';
import { getSkillDigest } from '../../../lib/agent-docs';

export const GET: APIRoute = () =>
  new Response(
    JSON.stringify(
      {
        $schema: 'https://schemas.agentskills.io/discovery/0.2.0/schema.json',
        skills: [
          {
            name: 'onwardpg',
            type: 'skill-md',
            description: 'Plan, revise, restack, and verify compatibility-aware PostgreSQL migrations with onwardpg.',
            url: '/.well-known/agent-skills/onwardpg/SKILL.md',
            digest: getSkillDigest(),
          },
        ],
      },
      null,
      2,
    ),
    {
      headers: {
        'Cache-Control': 'public, max-age=300',
        'Content-Type': 'application/json; charset=utf-8',
      },
    },
  );
