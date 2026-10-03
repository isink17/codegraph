from lib import full


def run():
    class C:
        g = (lambda full: full())(lambda: "param")
    return C.g
