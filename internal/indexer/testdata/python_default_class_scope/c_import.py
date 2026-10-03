from lib import full


def run():
    class C:
        from lib import NAME as full
        g = full
        h = full.upper()
    return C.h
