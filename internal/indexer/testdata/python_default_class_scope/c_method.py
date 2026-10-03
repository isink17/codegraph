from lib import full


def run():
    class C:
        full = 1

        def m(self):
            return full()
    return C().m()
