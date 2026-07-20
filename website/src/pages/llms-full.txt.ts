import type { APIRoute } from 'astro';
import { markdownResponse, renderLlmsFullTxt } from '../lib/agent-docs';

export const GET: APIRoute = () => markdownResponse(renderLlmsFullTxt());
