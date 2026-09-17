#VERSION: 1.42
#AUTHORS: dl-tool fixture
#
# Synthetic nova3 plugin for the T060 tests. The class name equals the file
# stem, every class attribute is a literal, and all nine friendly
# supported_categories keys are declared. It is never executed.


class legacy_plugin(object):
    name = "Legacy Tracker"
    url = "https://legacy-tracker.example"
    supported_categories = {
        'all': '0',
        'anime': '70',
        'books': '100',
        'games': '40',
        'movies': '20',
        'music': '30',
        'pictures': '90',
        'software': '80',
        'tv': '50',
    }

    def search(self, query, cat='all'):
        # A real plugin fetches and parses here. The importer must never
        # call it: a RuntimeError stands where a request would be made.
        raise RuntimeError("this file must never be executed")

    def download_torrent(self, info):
        raise RuntimeError("this file must never be executed")
