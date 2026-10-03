from lib import full


class C:
    full = lambda: "classattr"
    g = full()


def run():
    return C.g
