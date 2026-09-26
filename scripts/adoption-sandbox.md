# Blind adoption sandbox

Create a real, disposable existing project for a new-user exercise. Requirements:
Docker, Python 3, `psql`, npm (Node 24+ for Prisma), and `uv` for Django.
The sandbox creates its own PostgreSQL 18 container and a random localhost port;
it does not use any application database configured on the host.

```sh
go build -o /tmp/onwardpg-study ./cmd/onwardpg
python3 scripts/adoption-sandbox.py create \
  --root /tmp/onwardpg-study-django \
  --framework django \
  --binary /tmp/onwardpg-study
```

Framework choices: `django`, `drizzle`, `prisma`, or `sql`. Each starts with
100 customers. Drizzle uses matching 1.0.0-rc.4 ORM and Kit packages. Framework fixtures have a real baseline migration already
applied. The root must be a new directory outside this checkout.

The result contains:

- `app/`: models, existing migration history, and a product-change request.
- `public-docs/`: a snapshot of the quick start, public references, and examples.
- `bin/onwardpg`: the exact binary under study.
- `env.sh`: generated credentials and tool PATH; source it before using the app.
- `evidence/`: provisioning log and space for command transcripts and findings.
- `manifest.json`: exact container identity and assigned port.

Give a fresh agent only that root, a framework/background persona, and this task:

> Adopt onwardpg for the product change in APP-NOTES.md. Use the installed
> binary, its help, and the supplied public documentation. Keep models
> authoritative, preserve existing data, verify the bundle, and rehearse old
> and new writers during overlap using actual ORM calls. Execute each phase batch
> with its declared transaction boundary. Exercise live contract readiness using
> evidence about this sandbox only. Explain the framework migration-runner handoff.
> Record commands, outputs, exit codes, edits, and actionable feedback in
> evidence/. Do not read onwardpg implementation code or other agents' reports.
> Do not modify the product or use any database outside this sandbox.

Use a fresh agent with no inherited conversation each round. Keep transcripts
outside `app/`, where exporter mutation detection would otherwise observe them.
Classify operator mistakes and intentional safety decisions separately from
defects. Agent simulations are not evidence from actual human users.

To stop the exact owned database container while retaining files and evidence:

```sh
python3 scripts/adoption-sandbox.py down --root /tmp/onwardpg-study-django
```

The helper checks the container label against the manifest before removal.
Use this cleanup command after interrupted provisioning too. The directories
and database are isolated per run; agents still run on the host, so this is not
an OS security boundary for untrusted agent code.
