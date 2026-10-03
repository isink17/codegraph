from lib import full


def run():
    class C:
        full = lambda: "cls"

        def m(self, x=full()):
            return x
    return C().m()
