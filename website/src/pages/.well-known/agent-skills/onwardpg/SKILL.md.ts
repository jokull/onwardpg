import type { APIRoute } from 'astro';
import { getSkill, markdownResponse } from '../../../../lib/agent-docs';

export const GET: APIRoute = () => markdownResponse(getSkill());
