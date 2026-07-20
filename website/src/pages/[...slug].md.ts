import type { APIRoute, GetStaticPaths } from 'astro';
import { getAgentDocs, markdownResponse } from '../lib/agent-docs';

interface Props {
  source: string;
}

export const getStaticPaths: GetStaticPaths = () =>
  getAgentDocs().map((doc) => ({
    params: { slug: doc.slug },
    props: { source: doc.raw },
  }));

export const GET: APIRoute<Props> = ({ props }) => markdownResponse(props.source);
