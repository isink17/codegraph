from lib import full


class B:
    def __init_subclass__(cls, cb=None):
        cls.r = cb(lambda: "param")


def run():
    class C(B, cb=lambda full: full()):
        pass
    return C.r
