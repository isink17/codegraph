def café():
    return "module"


def run():
    from lib import thé as café
    return café()
