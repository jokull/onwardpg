import { PGlite } from '@electric-sql/pglite'
import { PGLiteSocketServer } from '@electric-sql/pglite-socket'

const HOST = '127.0.0.1'
const PGLITE_VERSION = '0.4.5'
const SOCKET_VERSION = '0.1.5'

const db = await PGlite.create({ dataDir: 'memory://' })
const server = new PGLiteSocketServer({
  db,
  host: HOST,
  port: 0,
  maxConnections: 8,
  idleTimeout: 0,
  debug: false,
  inspect: false,
})

let stopping = false
async function stop(code = 0) {
  if (stopping) return
  stopping = true
  try {
    await server.stop()
    await db.close()
  } catch (error) {
    console.error(error)
    code = 1
  }
  process.exit(code)
}

process.on('SIGINT', () => void stop())
process.on('SIGTERM', () => void stop())
process.on('uncaughtException', (error) => {
  console.error(error)
  void stop(1)
})
process.on('unhandledRejection', (error) => {
  console.error(error)
  void stop(1)
})

server.addEventListener('listening', (event) => {
  const detail = event.detail
  process.stdout.write(
    `${JSON.stringify({
      kind: 'ready',
      host: detail.host,
      port: detail.port,
      pglite_version: PGLITE_VERSION,
      socket_version: SOCKET_VERSION,
    })}\n`,
  )
})

await server.start()
