from lib import full


def run():
    match {"k": 1}:
        case {**full}:
            return full()
