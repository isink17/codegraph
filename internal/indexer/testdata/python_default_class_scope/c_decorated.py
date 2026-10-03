from lib import full


def keep(cls):
    return cls


def run():
    @keep
    class C:
        full = lambda: "decorated"
        g = full()
    return C.g
