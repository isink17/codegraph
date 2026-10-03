from lib import full


def run(flag=True):
    if flag:
        class C:
            full = lambda: "cls"

            def m(self, x=full()):
                return x
    else:
        class C:
            h = 1

            def m(self, x=None):
                return x
    return C().m()
