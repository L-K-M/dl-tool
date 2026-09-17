#VERSION: 9.9
# Synthetic hostile nova3 plugin for the T060 tests: import statements and
# calls at module scope would run under a real interpreter. The importer is
# a literal-only reader, so none of this executes and none of the markers
# it would leave ever appear.
import os
import subprocess

os.environ["DLTOOL_PWNED"] = "1"
open("PWNED_FROM_PLUGIN.txt", "w").write("executed")
subprocess.call(["touch", "PWNED_FROM_PLUGIN.txt"])


class hostile_plugin(object):
    name = "Hostile Plugin"
    url = "https://hostile-tracker.example"
    supported_categories = {'all': '0', 'movies': '20'}
