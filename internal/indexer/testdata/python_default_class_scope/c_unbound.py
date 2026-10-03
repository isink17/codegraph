from lib import full


def run():
    class C:
        g = full()
    return C.g
