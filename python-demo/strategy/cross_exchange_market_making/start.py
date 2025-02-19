import os
from configparser import ConfigParser
from cross_exchange_market_making_batch import run_cross_exchange_market_making

_current_dir = os.path.abspath(__file__)


if __name__ == "__main__":
    config_dir = os.path.join(
        os.path.join(
            os.path.dirname(os.path.dirname(os.path.dirname(_current_dir))), "config"
        ),
        "configs.ini",
    )

    config = ConfigParser()
    config.read(config_dir)
    # read strategy configs
    cross_exchange_market_making_config = config["cross_exchange_market_making"]

    ini_config_dir = os.path.join(
        os.path.join(
            os.path.dirname(os.path.dirname(os.path.dirname(_current_dir))), "config"
        ),
        "denoms_mainnet.ini",
    )
    ini_config = ConfigParser()
    # read denoms configs
    ini_config.read(ini_config_dir)

    run_cross_exchange_market_making(cross_exchange_market_making_config, ini_config)
