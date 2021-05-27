import React, { ChangeEvent } from 'react';
import { Checkbox as CheckboxField } from '@grafana/ui';
import { css, cx } from 'emotion';
import { QueryBuilderFieldProps } from './types';
import { onBuilderChange, debounceChange } from '.';

export const Checkbox = (props: QueryBuilderFieldProps) => {
  const onChange = (event: ChangeEvent<HTMLInputElement>) => {
    onBuilderChange(props, event.currentTarget.checked);
  };

  return (
    <CheckboxField
      label={props.label}
      description={props.description}
      value={props.options.builder}
      onChange={debounceChange(onChange, props)}
      className={cx(styles.checkbox)}
    />
  );
};

const styles = {
  checkbox: css`
    padding-right: 16px;
  `,
};
