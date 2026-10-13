# Forwards 0.0.0.0:9222 -> 127.0.0.1:9223 (Chromium ignores --remote-debugging-address)
import asyncio

async def pipe(r, w):
    try:
        while data := await r.read(65536):
            w.write(data); await w.drain()
    except Exception:
        pass
    finally:
        w.close()

async def handle(cr, cw):
    try:
        ur, uw = await asyncio.open_connection("127.0.0.1", 9223)
    except OSError:
        cw.close(); return
    await asyncio.gather(pipe(cr, uw), pipe(ur, cw))

async def main():
    srv = await asyncio.start_server(handle, "0.0.0.0", 9222)
    async with srv:
        await srv.serve_forever()

asyncio.run(main())
