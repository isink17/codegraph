from lib import full


def run():
    class C:
        full = lambda: "classattr"
        g = full()
    return C.g
