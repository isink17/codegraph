from lib import full


def run():
    class A:
        class B:
            full = lambda: "nested"
            g = full()
    return A.B.g
