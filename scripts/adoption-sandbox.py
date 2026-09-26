#!/usr/bin/env python3
"""Create disposable, populated projects for blind CLI adoption exercises."""

import argparse
import hashlib
import json
import os
import pathlib
import re
import secrets
import shlex
import shutil
import subprocess
import time

REPO = pathlib.Path(__file__).resolve().parents[1]
parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("action", choices=["create", "down"])
parser.add_argument(
    "--root",
    type=pathlib.Path,
    required=True,
    help="new sandbox directory, outside the onwardpg checkout",
)
parser.add_argument("--framework", choices=["drizzle", "django", "prisma", "sql"])
parser.add_argument("--binary", type=pathlib.Path, help="onwardpg executable to copy")
a = parser.parse_args()
root = a.root.resolve()
if root == REPO or REPO in root.parents:
    parser.error("sandbox root must be outside the onwardpg checkout")
if a.action == "down":
    manifest = json.loads((root / "manifest.json").read_text())
    container = manifest["container"]
    label = subprocess.check_output(
        [
            "docker",
            "inspect",
            "--format",
            '{{index .Config.Labels "onwardpg.adoption"}}',
            container,
        ],
        text=True,
    ).strip()
    if label != manifest["id"]:
        raise SystemExit(
            "container does not match this sandbox manifest; refusing cleanup"
        )
    subprocess.run(["docker", "rm", "-f", container], check=True)
    print("Removed sandbox database; retained project and evidence at", root)
    raise SystemExit(0)
if not a.framework or not a.binary or not a.binary.is_file():
    parser.error("create requires --framework and an existing --binary")
a.name = root.name
if not re.fullmatch(r"[a-z0-9][a-z0-9-]{0,50}", a.name):
    parser.error(
        "sandbox directory name must contain lowercase letters, digits, or hyphens"
    )
root.parent.mkdir(parents=True, exist_ok=True)
root.mkdir(mode=0o700)
app = root / "app"
app.mkdir()
evidence = root / "evidence"
evidence.mkdir()
docs = root / "public-docs"
docs.mkdir()
for name in ["README.md", "docs", "examples", "skills"]:
    src = REPO / name
    if src.is_dir():
        shutil.copytree(src, docs / name)
    else:
        shutil.copy2(src, docs / name)
for name in [
    "ownership-review.md",
    "security-review.md",
    "operational-rehearsals.md",
    "adoption-study.md",
    "change-report-2026-09-26.md",
]:
    (docs / "docs" / name).unlink(missing_ok=True)
shutil.copy2(REPO / "website/src/content/docs/index.md", docs / "quick-start.md")
(root / "bin").mkdir()
shutil.copy2(a.binary.resolve(), root / "bin/onwardpg")
password = secrets.token_hex(16)
sandbox_id = secrets.token_hex(6)
container = "onwardpg-adoption-" + a.name + "-" + sandbox_id
with open(evidence / "provision.log", "w") as log:

    def run(args, cwd=app, env=None):
        subprocess.run(
            args, cwd=cwd, env=env, stdout=log, stderr=subprocess.STDOUT, check=True
        )

    run(
        [
            "docker",
            "run",
            "-d",
            "--name",
            container,
            "--label",
            "onwardpg.adoption=" + sandbox_id,
            "-e",
            "POSTGRES_PASSWORD=" + password,
            "-p",
            "127.0.0.1::5432",
            "postgres:18",
        ]
    )
    port = (
        subprocess.check_output(["docker", "port", container, "5432/tcp"], text=True)
        .strip()
        .split(":")[-1]
    )
    (root / "manifest.json").write_text(
        json.dumps(
            {
                "id": sandbox_id,
                "name": a.name,
                "framework": a.framework,
                "binary_sha256": hashlib.sha256(a.binary.read_bytes()).hexdigest(),
                "container": container,
                "port": int(port),
                "app": str(app),
                "evidence": str(evidence),
                "docs": str(docs),
            },
            indent=2,
        )
        + "\n"
    )
    for attempt in range(60):
        if (
            subprocess.run(
                [
                    "docker",
                    "exec",
                    container,
                    "pg_isready",
                    "-h",
                    "127.0.0.1",
                    "-U",
                    "postgres",
                ],
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
                check=False,
            ).returncode
            == 0
        ):
            break
        time.sleep(0.5)
    else:
        raise RuntimeError("PostgreSQL readiness timeout")
    run(["docker", "exec", container, "createdb", "-U", "postgres", "app"])
    baseurl = f"postgres://postgres:{password}@127.0.0.1:{port}"
    env = dict(
        os.environ,
        DATABASE_URL=baseurl + "/app?sslmode=disable",
        PGHOST="127.0.0.1",
        PGPORT=port,
        PGUSER="postgres",
        PGPASSWORD=password,
        PGDATABASE="app",
    )
    envvalues = {
        key: env[key]
        for key in [
            "DATABASE_URL",
            "PGHOST",
            "PGPORT",
            "PGUSER",
            "PGPASSWORD",
            "PGDATABASE",
        ]
    }
    envvalues["ONWARDPG_SCRATCH_DATABASE_URL"] = baseurl + "/postgres?sslmode=disable"
    envvalues["ONWARDPG_DEV_DATABASE_URL"] = env["DATABASE_URL"]
    envvalues["PATH"] = str(root / "bin") + ":" + os.environ["PATH"]

    (root / "env.sh").write_text(
        "\n".join("export " + k + "=" + shlex.quote(v) for k, v in envvalues.items())
        + "\n"
    )
    (root / "env.sh").chmod(0o600)

    def write(name, body):
        p = app / name
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_text(body)

    if a.framework == "drizzle":
        write(
            "package.json",
            json.dumps(
                {
                    "name": "booking-app",
                    "private": True,
                    "type": "module",
                    "scripts": {
                        "db:generate": "drizzle-kit generate",
                        "db:migrate": "drizzle-kit migrate",
                    },
                    "dependencies": {"drizzle-orm": "1.0.0-rc.4", "pg": "8.23.0"},
                    "devDependencies": {"drizzle-kit": "1.0.0-rc.4"},
                },
                indent=2,
            ),
        )
        write(
            "schema.ts",
            "import { pgTable, serial, text } from 'drizzle-orm/pg-core';\nexport const customers = pgTable('customers', {\n  id: serial('id').primaryKey(),\n  displayName: text('display_name').notNull(),\n  email: text('email').notNull().unique(),\n});\n",
        )
        write(
            "drizzle.config.ts",
            "import { defineConfig } from 'drizzle-kit';\nexport default defineConfig({ dialect: 'postgresql', schema: './schema.ts', out: './drizzle', dbCredentials: { url: process.env.DATABASE_URL! } });\n",
        )
        run(["npm", "install", "--no-audit", "--no-fund"], env=env)
        run(["npm", "run", "db:generate"], env=env)
        run(["npm", "run", "db:migrate"], env=env)
        table = "customers"
    elif a.framework == "prisma":
        write(
            "package.json",
            json.dumps(
                {
                    "name": "booking-app",
                    "private": True,
                    "type": "module",
                    "devDependencies": {"prisma": "7.10.0"},
                },
                indent=2,
            ),
        )
        write(
            "prisma.config.ts",
            "import { defineConfig, env } from 'prisma/config';\nexport default defineConfig({ schema: './prisma/schema.prisma', migrations: { path: './prisma/migrations' }, datasource: { url: env('DATABASE_URL') } });\n",
        )
        write(
            "prisma/schema.prisma",
            """datasource db {
      provider = "postgresql"
    }
    model Customer {
      id Int @id @default(autoincrement())
      display_name String
      email String @unique
      @@map("customers")
    }
    """,
        )
        run(["npm", "install", "--no-audit", "--no-fund"], env=env)
        ddl = subprocess.check_output(
            [
                "npx",
                "--no-install",
                "prisma",
                "migrate",
                "diff",
                "--from-empty",
                "--to-schema",
                "prisma/schema.prisma",
                "--script",
            ],
            cwd=app,
            env=env,
            stderr=log,
        )
        write("prisma/migrations/20260926000000_baseline/migration.sql", ddl.decode())
        write("prisma/migrations/migration_lock.toml", 'provider = "postgresql"\n')
        run(["npx", "--no-install", "prisma", "migrate", "deploy"], env=env)
        table = "customers"
    elif a.framework == "django":
        write(
            "manage.py",
            "#!/usr/bin/env python3\nimport os, sys\nos.environ.setdefault('DJANGO_SETTINGS_MODULE', 'settings')\nfrom django.core.management import execute_from_command_line\nexecute_from_command_line(sys.argv)\n",
        )
        write(
            "settings.py",
            "import os\nSECRET_KEY='disposable-adoption-fixture'\nINSTALLED_APPS=['bookings']\nDEFAULT_AUTO_FIELD='django.db.models.BigAutoField'\nDATABASES={'default':{'ENGINE':'django.db.backends.postgresql','NAME':os.environ['PGDATABASE'],'HOST':os.environ['PGHOST'],'PORT':os.environ['PGPORT'],'USER':os.environ['PGUSER'],'PASSWORD':os.environ['PGPASSWORD']}}\n",
        )
        write("bookings/__init__.py", "")
        write(
            "bookings/models.py",
            "from django.db import models\n\nclass Customer(models.Model):\n    display_name = models.TextField()\n    email = models.TextField(unique=True)\n",
        )
        run(["uv", "venv", str(root / "venv"), "--python", "3.13"], env=env)
        python = str(root / "venv/bin/python")
        run(
            [
                "uv",
                "pip",
                "install",
                "--python",
                python,
                "Django==5.2.17",
                "psycopg[binary]==3.3.6",
            ],
            env=env,
        )
        run([python, "manage.py", "makemigrations", "bookings"], env=env)
        run([python, "manage.py", "migrate"], env=env)
        run(["uv", "pip", "freeze", "--python", python], env=env)
        with (root / "env.sh").open("a") as f:
            f.write(
                "export PATH="
                + shlex.quote(
                    str(root / "bin")
                    + ":"
                    + str(root / "venv/bin")
                    + ":"
                    + os.environ["PATH"]
                )
                + "\n"
            )
        table = "bookings_customer"
    else:
        write(
            "schema.sql",
            "CREATE TABLE customers (id bigint GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY, display_name text NOT NULL, email text NOT NULL UNIQUE);\n",
        )
        run(["psql", "-X", "-v", "ON_ERROR_STOP=1", "-f", "schema.sql"], env=env)
        table = "customers"
    run(
        [
            "psql",
            "-X",
            "-v",
            "ON_ERROR_STOP=1",
            "-c",
            f"INSERT INTO {table} (display_name,email) SELECT 'Customer ' || g, 'person-' || g || '@example.test' FROM generate_series(1,100) g;",
        ],
        env=env,
    )
    write(
        "APP-NOTES.md",
        f"""# Existing application

    This is a working {a.framework} project with 100 existing customers in its local
    PostgreSQL database. Each has an ID, display_name, and unique email.
    The current application reads and writes display_name. Database access is
    configured by the environment in ../env.sh. Existing framework migration history
    (where present) has already been applied to this database.

    Product change: rename the stored display_name to full_name without breaking
    old application instances during a rolling deployment. Preserve all customers.
    Then add a required account_status with 'active' for existing customers; the new
    application will supply this field for new customers. You may split changes
    into releases if needed. Decide and document how future framework migrations
    and onwardpg fit together. The framework models remain the desired schema.

    Local documentation: ../public-docs/quick-start.md and
    ../public-docs/examples/frameworks/README.md (links to each framework recipe).
    """.replace("\n    ", "\n"),
    )
    print(
        json.dumps(
            {
                "root": str(root),
                "framework": a.framework,
                "binary_sha256": hashlib.sha256(a.binary.read_bytes()).hexdigest(),
                "container": container,
                "next": "source " + str(root / "env.sh") + "; cd " + str(app),
            }
        )
    )
