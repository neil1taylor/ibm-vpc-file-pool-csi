import React from 'react';
import { Label } from '@patternfly/react-core';
import {
  CheckCircleIcon,
  InProgressIcon,
  ExclamationTriangleIcon,
  ExclamationCircleIcon,
  UnknownIcon,
} from '@patternfly/react-icons';
import { phaseBadgeColor, BadgeColor } from '../../utils/resource-helpers';

const colorToLabelColor: Record<BadgeColor, 'green' | 'blue' | 'orange' | 'red' | 'grey'> = {
  green: 'green',
  blue: 'blue',
  orange: 'orange',
  red: 'red',
  grey: 'grey',
};

const colorToIcon: Record<BadgeColor, React.ReactNode> = {
  green: <CheckCircleIcon />,
  blue: <InProgressIcon />,
  orange: <ExclamationTriangleIcon />,
  red: <ExclamationCircleIcon />,
  grey: <UnknownIcon />,
};

interface PhaseStatusProps {
  phase: string | undefined;
}

const PhaseStatus: React.FC<PhaseStatusProps> = ({ phase }) => {
  const color = phaseBadgeColor(phase);
  return (
    <Label color={colorToLabelColor[color]} icon={colorToIcon[color]}>
      {phase || 'Unknown'}
    </Label>
  );
};

export default PhaseStatus;
