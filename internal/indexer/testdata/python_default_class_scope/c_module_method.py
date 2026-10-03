from lib import full


class C:
    full = 1

    def m(self):
        return full()


def run():
    return C().m()
