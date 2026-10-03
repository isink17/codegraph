def café():
    return "module"


def run():
    def café():
        return "nested"
    return café()
