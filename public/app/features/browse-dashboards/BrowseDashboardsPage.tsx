import { css } from '@emotion/css';
import React, { memo, useMemo } from 'react';

import { locationSearchToObject } from '@grafana/runtime';
import { useStyles2 } from '@grafana/ui';
import { Page } from 'app/core/components/Page/Page';
import { GrafanaRouteComponentProps } from 'app/core/navigation/types';

import { buildNavModel } from '../folders/state/navModel';
import { parseRouteParams } from '../search/utils';

import { skipToken, useGetFolderQuery } from './api/browseDashboardsAPI';
import { BrowseActions } from './components/BrowseActions';
import { BrowseView } from './components/BrowseView';
import { SearchView } from './components/SearchView';

export interface BrowseDashboardsPageRouteParams {
  uid?: string;
  slug?: string;
}

interface Props extends GrafanaRouteComponentProps<BrowseDashboardsPageRouteParams> {}

// New Browse/Manage/Search Dashboards views for nested folders

export const BrowseDashboardsPage = memo(({ match, location }: Props) => {
  const styles = useStyles2(getStyles);
  const { uid: folderUID } = match.params;

  const searchState = useMemo(() => {
    return parseRouteParams(locationSearchToObject(location.search));
  }, [location.search]);

  const { data: folderDTO } = useGetFolderQuery(folderUID ?? skipToken);
  const navModel = useMemo(() => (folderDTO ? buildNavModel(folderDTO) : undefined), [folderDTO]);

  return (
    <Page navId="dashboards/browse" pageNav={navModel}>
      <Page.Contents className={styles.pageContents}>
        <BrowseActions />

        {folderDTO && <pre>{JSON.stringify(folderDTO, null, 2)}</pre>}

        {searchState.query ? <SearchView searchState={searchState} /> : <BrowseView folderUID={folderUID} />}
      </Page.Contents>
    </Page>
  );
});

const getStyles = () => ({
  pageContents: css({
    display: 'grid',
    gridTemplateRows: 'auto 1fr',
    height: '100%',
  }),
});

BrowseDashboardsPage.displayName = 'BrowseDashboardsPage';
