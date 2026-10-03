from lib import full


def run():
    class C: full = lambda: "cls"; g = full()
    return C.g
