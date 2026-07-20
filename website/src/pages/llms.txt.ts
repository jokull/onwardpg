import type { APIRoute } from 'astro';
import { markdownResponse, renderLlmsTxt } from '../lib/agent-docs';

export const GET: APIRoute = () => markdownResponse(renderLlmsTxt());
