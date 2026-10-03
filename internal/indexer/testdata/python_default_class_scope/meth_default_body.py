from lib import full


def run():
    class C:
        full = lambda: "cls"

        def m(self, x=full(),
              y=None):
            return full()
    return C().m()
