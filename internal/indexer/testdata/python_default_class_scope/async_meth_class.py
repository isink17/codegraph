from lib import full


def run():
    class C:
        full = lambda: "cls"

        async def m(self, x=full()):
            return x
    import asyncio
    return asyncio.run(C().m())
