import type { APIRoute, GetStaticPaths } from 'astro';
import { getSkillReferences, markdownResponse } from '../../lib/agent-docs';

interface Props {
  source: string;
}

export const getStaticPaths: GetStaticPaths = () =>
  getSkillReferences().map((reference) => ({
    params: { name: reference.name },
    props: { source: reference.source },
  }));

export const GET: APIRoute<Props> = ({ props }) => markdownResponse(props.source);
