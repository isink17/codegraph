from lib import full


class C:
    full = lambda: "cls"

    def m(self, x=full()):
        return x


def run():
    return C().m()
