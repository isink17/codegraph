from lib import Box, full


def run():
    match Box():
        case Box(full=v):
            return full()
