from lib import full


def run():
    match 1:
        case x if full():
            return full()
